package tns

import (
	"encoding/binary"
	"errors"
)

// WriteBuffer builds TTC-encoded requests and sends them as one or more
// DATA packets sized to the negotiated SDU. Like ReadBuffer it keeps a
// sticky error.
type WriteBuffer struct {
	transport *Transport
	caps      *Capabilities

	data        []byte
	pos         int
	maxSize     int
	packetType  uint8
	packetFlags uint8
	dataFlags   uint16
	seqNum      uint8
	packetSent  bool
	err         error
}

// NewWriteBuffer creates a write buffer sized to the current SDU.
func NewWriteBuffer(t *Transport, caps *Capabilities) *WriteBuffer {
	w := &WriteBuffer{transport: t, caps: caps}
	w.SizeForSDU()
	return w
}

// Err returns the sticky error.
func (w *WriteBuffer) Err() error { return w.err }

// PacketSent reports whether any packet of the current request was sent.
func (w *WriteBuffer) PacketSent() bool { return w.packetSent }

// SetPacketSent overrides the packet-sent marker.
func (w *WriteBuffer) SetPacketSent(v bool) { w.packetSent = v }

// SizeForSDU resizes the buffer to the negotiated SDU.
func (w *WriteBuffer) SizeForSDU() {
	w.maxSize = int(w.caps.SDU) - w.transport.Security().Overhead()
	if cap(w.data) < w.maxSize {
		w.data = make([]byte, w.maxSize)
	}
	w.data = w.data[:w.maxSize]
}

// StartRequest begins a new packet of the given type.
func (w *WriteBuffer) StartRequest(packetType, packetFlags uint8, dataFlags uint16) {
	w.err = nil
	w.packetSent = false
	w.packetType = packetType
	w.packetFlags = packetFlags
	w.pos = PacketHeaderSize
	if packetType == PacketTypeData {
		w.dataFlags = dataFlags
		w.pos += 2
	}
}

// OrDataFlags ORs additional data flags into the pending packet.
func (w *WriteBuffer) OrDataFlags(f uint16) { w.dataFlags |= f }

func (w *WriteBuffer) sendPacket(final bool) {
	if w.err != nil {
		return
	}
	size := w.pos
	if w.caps.ProtocolVersion >= VersionMinLargeSDU {
		binary.BigEndian.PutUint32(w.data[0:], uint32(size))
	} else {
		binary.BigEndian.PutUint16(w.data[0:], uint16(size))
		binary.BigEndian.PutUint16(w.data[2:], 0)
	}
	w.data[4] = w.packetType
	w.data[5] = w.packetFlags
	binary.BigEndian.PutUint16(w.data[6:], 0)
	if w.packetType == PacketTypeData {
		binary.BigEndian.PutUint16(w.data[8:], w.dataFlags)
	}
	if err := w.transport.WritePacket(w.data[:size]); err != nil {
		w.err = err
		return
	}
	w.packetSent = true
	w.pos = PacketHeaderSize
	if !final && w.packetType == PacketTypeData {
		w.pos += 2
	}
}

// EndRequest flushes the final packet of the request.
func (w *WriteBuffer) EndRequest() error {
	if w.pos > PacketHeaderSize {
		w.sendPacket(true)
	}
	return w.err
}

func (w *WriteBuffer) ensure(n int) bool {
	if w.err != nil {
		return false
	}
	if w.pos+n > w.maxSize {
		w.sendPacket(false)
	}
	return w.err == nil
}

// WriteUint8 writes one byte.
func (w *WriteBuffer) WriteUint8(v uint8) {
	if !w.ensure(1) {
		return
	}
	w.data[w.pos] = v
	w.pos++
}

// WriteUint16BE writes a raw big-endian 16-bit integer.
func (w *WriteBuffer) WriteUint16BE(v uint16) {
	if !w.ensure(2) {
		return
	}
	binary.BigEndian.PutUint16(w.data[w.pos:], v)
	w.pos += 2
}

// WriteUint16LE writes a raw little-endian 16-bit integer.
func (w *WriteBuffer) WriteUint16LE(v uint16) {
	if !w.ensure(2) {
		return
	}
	binary.LittleEndian.PutUint16(w.data[w.pos:], v)
	w.pos += 2
}

// WriteUint32BE writes a raw big-endian 32-bit integer.
func (w *WriteBuffer) WriteUint32BE(v uint32) {
	if !w.ensure(4) {
		return
	}
	binary.BigEndian.PutUint32(w.data[w.pos:], v)
	w.pos += 4
}

// WriteUint64BE writes a raw big-endian 64-bit integer.
func (w *WriteBuffer) WriteUint64BE(v uint64) {
	if !w.ensure(8) {
		return
	}
	binary.BigEndian.PutUint64(w.data[w.pos:], v)
	w.pos += 8
}

// WriteUB2 writes a universal-format 16-bit integer.
func (w *WriteBuffer) WriteUB2(v uint16) {
	switch {
	case v == 0:
		w.WriteUint8(0)
	case v <= 0xff:
		w.WriteUint8(1)
		w.WriteUint8(uint8(v))
	default:
		w.WriteUint8(2)
		w.WriteUint16BE(v)
	}
}

// WriteUB4 writes a universal-format 32-bit integer.
func (w *WriteBuffer) WriteUB4(v uint32) {
	switch {
	case v == 0:
		w.WriteUint8(0)
	case v <= 0xff:
		w.WriteUint8(1)
		w.WriteUint8(uint8(v))
	case v <= 0xffff:
		w.WriteUint8(2)
		w.WriteUint16BE(uint16(v))
	default:
		w.WriteUint8(4)
		w.WriteUint32BE(v)
	}
}

// WriteUB8 writes a universal-format 64-bit integer.
func (w *WriteBuffer) WriteUB8(v uint64) {
	switch {
	case v == 0:
		w.WriteUint8(0)
	case v <= 0xff:
		w.WriteUint8(1)
		w.WriteUint8(uint8(v))
	case v <= 0xffff:
		w.WriteUint8(2)
		w.WriteUint16BE(uint16(v))
	case v <= 0xffffffff:
		w.WriteUint8(4)
		w.WriteUint32BE(uint32(v))
	default:
		w.WriteUint8(8)
		w.WriteUint64BE(v)
	}
}

// WriteSB4 writes a universal-format signed 32-bit integer.
func (w *WriteBuffer) WriteSB4(v int32) {
	var sign uint8
	if v < 0 {
		v = -v
		sign = 0x80
	}
	switch {
	case v == 0:
		w.WriteUint8(0)
	case v <= 0xff:
		w.WriteUint8(1 | sign)
		w.WriteUint8(uint8(v))
	case v <= 0xffff:
		w.WriteUint8(2 | sign)
		w.WriteUint16BE(uint16(v))
	default:
		w.WriteUint8(4 | sign)
		w.WriteUint32BE(uint32(v))
	}
}

// WriteRaw writes raw bytes, splitting across packets as needed.
func (w *WriteBuffer) WriteRaw(b []byte) {
	for len(b) > 0 && w.err == nil {
		avail := w.maxSize - w.pos
		if avail == 0 {
			w.sendPacket(false)
			continue
		}
		n := len(b)
		if n > avail {
			n = avail
		}
		copy(w.data[w.pos:], b[:n])
		w.pos += n
		b = b[n:]
	}
}

// WriteBytes writes raw bytes (alias of WriteRaw).
func (w *WriteBuffer) WriteBytes(b []byte) { w.WriteRaw(b) }

// WriteStr writes a string as raw UTF-8 bytes.
func (w *WriteBuffer) WriteStr(s string) { w.WriteRaw([]byte(s)) }

// WriteBytesWithLength writes a value prefixed by its TTC length encoding
// (short form up to 252 bytes, else chunked form).
func (w *WriteBuffer) WriteBytesWithLength(b []byte) {
	n := len(b)
	if n <= MaxShortLength {
		w.WriteUint8(uint8(n))
		if n > 0 {
			w.WriteRaw(b)
		}
		return
	}
	w.WriteUint8(LongLengthIndicator)
	for len(b) > 0 {
		c := len(b)
		if c > ChunkSize {
			c = ChunkSize
		}
		w.WriteUB4(uint32(c))
		w.WriteRaw(b[:c])
		b = b[c:]
	}
	w.WriteUB4(0)
}

// WriteBytesWithTwoLengths writes a ub4 length followed by a
// length-prefixed value (nil writes only a zero length).
func (w *WriteBuffer) WriteBytesWithTwoLengths(b []byte) {
	if b == nil {
		w.WriteUB4(0)
		return
	}
	w.WriteUB4(uint32(len(b)))
	if len(b) > 0 {
		w.WriteBytesWithLength(b)
	}
}

// WriteStrWithTwoLengths writes a string with the two-length encoding.
func (w *WriteBuffer) WriteStrWithTwoLengths(s string) {
	w.WriteBytesWithTwoLengths([]byte(s))
}

// WriteSeqNum writes the next request sequence number.
func (w *WriteBuffer) WriteSeqNum() {
	w.seqNum++
	if w.seqNum == 0 {
		w.seqNum = 1
	}
	w.WriteUint8(w.seqNum)
}

// Caps returns the capability set.
func (w *WriteBuffer) Caps() *Capabilities { return w.caps }

// Caps returns the capability set.
func (r *ReadBuffer) Caps() *Capabilities { return r.caps }

// Transport returns the transport.
func (r *ReadBuffer) Transport() *Transport { return r.transport }

var errBufferTooSmall = errors.New("tns: write buffer too small")
