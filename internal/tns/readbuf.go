package tns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
)

// ErrMarkerDetected is returned when a MARKER packet arrives in the middle
// of a response; the protocol layer resets the connection state.
var ErrMarkerDetected = errors.New("tns: marker detected")

// ReadBuffer reads TTC-encoded values from a sequence of DATA packets. It
// uses a sticky error: once a read fails every subsequent read is a no-op
// and Err() reports the failure. Callers check Err() at natural points.
type ReadBuffer struct {
	transport *Transport
	caps      *Capabilities

	data []byte // current packet buffer
	pos  int
	size int

	current       *Packet
	saved         []*Packet
	nextPacketPos int

	scratch []byte // for values split across packets
	chunk   []byte // for chunked (long) reads

	pendingErrorNum      uint32
	checkRequestBoundary bool
	err                  error
}

// NewReadBuffer creates a read buffer over the transport.
func NewReadBuffer(t *Transport, caps *Capabilities) *ReadBuffer {
	return &ReadBuffer{transport: t, caps: caps}
}

// Err returns the sticky error, if any.
func (r *ReadBuffer) Err() error { return r.err }

// SetErr records an error if none is set.
func (r *ReadBuffer) SetErr(err error) {
	if r.err == nil && err != nil {
		r.err = err
	}
}

// ClearErr resets the sticky error (used after a controlled marker reset).
func (r *ReadBuffer) ClearErr() { r.err = nil }

// PendingErrorNum returns an in-band error number received via control
// packets (0 if none).
func (r *ReadBuffer) PendingErrorNum() uint32 { return r.pendingErrorNum }

// ClearPendingError clears the in-band error.
func (r *ReadBuffer) ClearPendingError() { r.pendingErrorNum = 0 }

// SetCheckRequestBoundary controls whether packets are accumulated until
// the end-of-response flag before processing begins.
func (r *ReadBuffer) SetCheckRequestBoundary(v bool) { r.checkRequestBoundary = v }

// CurrentPacket returns the packet being processed.
func (r *ReadBuffer) CurrentPacket() *Packet { return r.current }

// ResetPackets discards saved packets; called before a request is sent.
func (r *ReadBuffer) ResetPackets() {
	r.saved = r.saved[:0]
	r.nextPacketPos = 0
	r.current = nil
	r.data = nil
	r.pos, r.size = 0, 0
}

// Pos returns the position within the current packet.
func (r *ReadBuffer) Pos() int { return r.pos }

// BytesLeft returns the bytes remaining in the current packet.
func (r *ReadBuffer) BytesLeft() int { return r.size - r.pos }

func (r *ReadBuffer) processPacket(p *Packet) (notify bool) {
	switch p.Type {
	case PacketTypeControl:
		if len(p.Buf) >= PacketHeaderSize+2 {
			ct := binary.BigEndian.Uint16(p.Buf[PacketHeaderSize:])
			switch ct {
			case ControlTypeResetOOB:
				r.caps.SupportsOOB = false
			case ControlTypeInbandNotification:
				if len(p.Buf) >= PacketHeaderSize+10 {
					r.pendingErrorNum = binary.BigEndian.Uint32(p.Buf[PacketHeaderSize+6:])
				}
			}
		}
		return false
	default:
		r.saved = append(r.saved, p)
		return p.Type != PacketTypeData || !r.checkRequestBoundary || p.HasEndOfResponse()
	}
}

func (r *ReadBuffer) startPacket() {
	r.current = r.saved[r.nextPacketPos]
	r.nextPacketPos++
	r.data = r.current.Buf
	r.size = len(r.data)
	r.pos = PacketHeaderSize
	if r.current.Type == PacketTypeData {
		flags := r.readUint16BEraw()
		if flags == DataFlagsEOF {
			r.pendingErrorNum = ErrSessionShutdown
		}
	}
}

func (r *ReadBuffer) readUint16BEraw() uint16 {
	if r.pos+2 > r.size {
		r.SetErr(errors.New("tns: truncated packet"))
		return 0
	}
	v := binary.BigEndian.Uint16(r.data[r.pos:])
	r.pos += 2
	return v
}

// WaitForPackets advances to the next packet, reading from the transport
// if necessary. If checkMarker is set and a MARKER packet is encountered,
// ErrMarkerDetected is recorded.
func (r *ReadBuffer) WaitForPackets(checkMarker bool) error {
	if r.err != nil {
		return r.err
	}
	if r.nextPacketPos >= len(r.saved) {
		for {
			p, err := r.transport.ReadPacket()
			if err != nil {
				r.SetErr(err)
				return err
			}
			if r.processPacket(p) {
				break
			}
			if err := r.checkConnected(); err != nil {
				r.SetErr(err)
				return err
			}
		}
	}
	r.startPacket()
	if checkMarker && r.current.Type == PacketTypeMarker {
		r.SetErr(ErrMarkerDetected)
		return ErrMarkerDetected
	}
	return r.err
}

func (r *ReadBuffer) checkConnected() error {
	switch r.pendingErrorNum {
	case 0, ErrSessionShutdown, ErrInbandMessage:
		return nil
	case ErrExceededIdleTime:
		return fmt.Errorf("oracle: ORA-%05d: the session exceeded its idle time limit", r.pendingErrorNum)
	default:
		return fmt.Errorf("oracle: unsupported in-band notification ORA-%05d", r.pendingErrorNum)
	}
}

// getRaw returns n bytes, spanning packets if required. The returned slice
// is only valid until the next read.
func (r *ReadBuffer) getRaw(n int) []byte {
	if r.err != nil {
		return nil
	}
	if r.pos == r.size {
		if err := r.WaitForPackets(true); err != nil {
			return nil
		}
	}
	left := r.size - r.pos
	if n <= left {
		out := r.data[r.pos : r.pos+n]
		r.pos += n
		return out
	}
	// split across packets: assemble into scratch
	if cap(r.scratch) < n {
		r.scratch = make([]byte, n)
	}
	r.scratch = r.scratch[:n]
	copied := copy(r.scratch, r.data[r.pos:r.size])
	r.pos = r.size
	for copied < n {
		if err := r.WaitForPackets(true); err != nil {
			return nil
		}
		take := n - copied
		if take > r.size-r.pos {
			take = r.size - r.pos
		}
		copy(r.scratch[copied:], r.data[r.pos:r.pos+take])
		r.pos += take
		copied += take
	}
	return r.scratch
}

// SkipRawBytes skips n bytes.
func (r *ReadBuffer) SkipRawBytes(n int) {
	for n > 0 && r.err == nil {
		take := r.BytesLeft()
		if take == 0 {
			if err := r.WaitForPackets(true); err != nil {
				return
			}
			continue
		}
		if take > n {
			take = n
		}
		r.pos += take
		n -= take
	}
}

// ReadRawBytes returns n bytes (copied out of packet buffers only when the
// value spans packets).
func (r *ReadBuffer) ReadRawBytes(n int) []byte { return r.getRaw(n) }

func (r *ReadBuffer) intLengthAndSign(maxLen int, allowNeg bool) (length int, neg bool) {
	b := r.getRaw(1)
	if b == nil {
		return 0, false
	}
	if b[0]&0x80 != 0 {
		if !allowNeg {
			r.SetErr(errors.New("tns: unexpected negative integer"))
			return 0, false
		}
		neg = true
		length = int(b[0] & 0x7f)
	} else {
		length = int(b[0])
	}
	if length > maxLen {
		r.SetErr(fmt.Errorf("tns: integer length %d exceeds maximum %d", length, maxLen))
		return 0, false
	}
	return length, neg
}

func decodeInteger(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// ReadUB1 reads an unsigned byte.
func (r *ReadBuffer) ReadUB1() uint8 {
	b := r.getRaw(1)
	if b == nil {
		return 0
	}
	return b[0]
}

// ReadSB1 reads a signed byte.
func (r *ReadBuffer) ReadSB1() int8 { return int8(r.ReadUB1()) }

func (r *ReadBuffer) readUnsigned(maxLen int) uint64 {
	length, _ := r.intLengthAndSign(maxLen, false)
	if length == 0 || r.err != nil {
		return 0
	}
	b := r.getRaw(length)
	if b == nil {
		return 0
	}
	return decodeInteger(b)
}

func (r *ReadBuffer) readSigned(maxLen int) int64 {
	length, neg := r.intLengthAndSign(maxLen, true)
	if length == 0 || r.err != nil {
		return 0
	}
	b := r.getRaw(length)
	if b == nil {
		return 0
	}
	v := int64(decodeInteger(b))
	if neg {
		v = -v
	}
	return v
}

// ReadUB2 reads a universal-format unsigned 16-bit integer.
func (r *ReadBuffer) ReadUB2() uint16 { return uint16(r.readUnsigned(2)) }

// ReadUB4 reads a universal-format unsigned 32-bit integer.
func (r *ReadBuffer) ReadUB4() uint32 { return uint32(r.readUnsigned(4)) }

// ReadUB8 reads a universal-format unsigned 64-bit integer.
func (r *ReadBuffer) ReadUB8() uint64 { return r.readUnsigned(8) }

// ReadSB2 reads a universal-format signed 16-bit integer.
func (r *ReadBuffer) ReadSB2() int16 { return int16(r.readSigned(2)) }

// ReadSB4 reads a universal-format signed 32-bit integer.
func (r *ReadBuffer) ReadSB4() int32 { return int32(r.readSigned(4)) }

// ReadSB8 reads a universal-format signed 64-bit integer.
func (r *ReadBuffer) ReadSB8() int64 { return r.readSigned(8) }

// SkipUB1 skips one byte.
func (r *ReadBuffer) SkipUB1() { r.getRaw(1) }

// SkipUB2 skips a universal 16-bit integer.
func (r *ReadBuffer) SkipUB2() { r.readUnsigned(2) }

// SkipUB4 skips a universal 32-bit integer.
func (r *ReadBuffer) SkipUB4() { r.readUnsigned(4) }

// SkipUB8 skips a universal 64-bit integer.
func (r *ReadBuffer) SkipUB8() { r.readUnsigned(8) }

// SkipSB4 skips a universal signed 32-bit integer.
func (r *ReadBuffer) SkipSB4() { r.readSigned(4) }

// ReadUint16BE reads a raw big-endian 16-bit integer.
func (r *ReadBuffer) ReadUint16BE() uint16 {
	b := r.getRaw(2)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}

// ReadUint16LE reads a raw little-endian 16-bit integer.
func (r *ReadBuffer) ReadUint16LE() uint16 {
	b := r.getRaw(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

// ReadUint32BE reads a raw big-endian 32-bit integer.
func (r *ReadBuffer) ReadUint32BE() uint32 {
	b := r.getRaw(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

// ReadUint64BE reads a raw big-endian 64-bit integer.
func (r *ReadBuffer) ReadUint64BE() uint64 {
	b := r.getRaw(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

// ReadRawBytesAndLength reads a length-prefixed value. The first byte is the
// length; 0 or 255 means NULL (nil returned); 254 means a chunked value made
// of ub4-length chunks terminated by a zero length. The returned slice is
// valid until the next read.
func (r *ReadBuffer) ReadRawBytesAndLength() []byte {
	length := r.ReadUB1()
	if r.err != nil {
		return nil
	}
	if length == 0 || length == NullLengthIndicator {
		return nil
	}
	if length != LongLengthIndicator {
		return r.getRaw(int(length))
	}
	r.chunk = r.chunk[:0]
	for {
		n := r.ReadUB4()
		if r.err != nil {
			return nil
		}
		if n == 0 {
			break
		}
		part := r.getRaw(int(n))
		if part == nil {
			return nil
		}
		r.chunk = append(r.chunk, part...)
	}
	return r.chunk
}

// ReadBytes reads a length-prefixed value and copies it out.
func (r *ReadBuffer) ReadBytes() []byte {
	b := r.ReadRawBytesAndLength()
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// ReadBytesWithLength reads a ub4 length then, if non-zero, a
// length-prefixed value.
func (r *ReadBuffer) ReadBytesWithLength() []byte {
	n := r.ReadUB4()
	if n > 0 {
		return r.ReadBytes()
	}
	return nil
}

// ReadStr reads a length-prefixed string in the database character set
// (UTF-8; CS form NCHAR values are UTF-16BE and are transcoded).
func (r *ReadBuffer) ReadStr(nchar bool) (string, bool) {
	b := r.ReadRawBytesAndLength()
	if b == nil {
		return "", false
	}
	if nchar {
		return DecodeUTF16BE(b), true
	}
	return string(b), true
}

// ReadStrWithLength reads a ub4 length then a string.
func (r *ReadBuffer) ReadStrWithLength() string {
	n := r.ReadUB4()
	if n == 0 {
		return ""
	}
	s, _ := r.ReadStr(false)
	return s
}

// ReadNullTerminatedBytes reads up to and including a NUL within the
// current packet.
func (r *ReadBuffer) ReadNullTerminatedBytes() []byte {
	start := r.pos
	end := r.pos
	for end < r.size && r.data[end] != 0 {
		end++
	}
	r.pos = end + 1
	if r.pos > r.size {
		r.pos = r.size
	}
	out := make([]byte, r.pos-start)
	copy(out, r.data[start:r.pos])
	return out
}

// SkipBytes skips a length-prefixed value.
func (r *ReadBuffer) SkipBytes() {
	length := r.ReadUB1()
	if r.err != nil {
		return
	}
	if length != LongLengthIndicator {
		r.SkipRawBytes(int(length))
		return
	}
	for {
		n := r.ReadUB4()
		if n == 0 || r.err != nil {
			return
		}
		r.SkipRawBytes(int(n))
	}
}

// SkipBytesWithLength skips a ub4-length-prefixed value.
func (r *ReadBuffer) SkipBytesWithLength() {
	n := r.ReadUB4()
	if n > 0 {
		r.SkipBytes()
	}
}

// Rowid is a physical Oracle rowid.
type Rowid struct {
	RBA         uint32
	PartitionID uint16
	BlockNum    uint32
	SlotNum     uint16
}

// ReadRowid reads a rowid structure.
func (r *ReadBuffer) ReadRowid() Rowid {
	var id Rowid
	id.RBA = r.ReadUB4()
	id.PartitionID = r.ReadUB2()
	r.SkipUB1()
	id.BlockNum = r.ReadUB4()
	id.SlotNum = r.ReadUB2()
	return id
}

// DecodeUTF16BE transcodes UTF-16BE bytes to a Go string.
func DecodeUTF16BE(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}
