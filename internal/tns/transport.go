package tns

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// ErrConnectionClosed is returned when the transport has been closed or the
// server has gone away.
var ErrConnectionClosed = errors.New("oracle: connection closed")

// Transport is a TNS packet transport over a TCP (optionally TLS) socket.
type Transport struct {
	conn           net.Conn
	fullPacketSize bool // true once the ACCEPT packet establishes large SDU
	maxPacketSize  int
	closed         bool
	Trace          func(format string, args ...any)
	readTimeout    time.Duration
	hdr            [PacketHeaderSize]byte
	writeMu        sync.Mutex
	security       *Security
}

// SetSecurity activates packet encryption / checksumming for all
// subsequent DATA packets.
func (t *Transport) SetSecurity(s *Security) { t.security = s }

// Security returns the negotiated packet protection (nil if none).
func (t *Transport) Security() *Security { return t.security }

// dataPayloadOffset is where a DATA packet's payload starts (8-byte
// header + 2-byte data flags).
const dataPayloadOffset = PacketHeaderSize + 2

// NewTransport wraps an established socket.
func NewTransport(conn net.Conn) *Transport {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
	}
	return &Transport{conn: conn, maxPacketSize: DefaultSDU}
}

// SetMaxPacketSize records the negotiated SDU.
func (t *Transport) SetMaxPacketSize(n int) { t.maxPacketSize = n }

// SetFullPacketSize switches to 32-bit packet lengths (protocol >= 315).
func (t *Transport) SetFullPacketSize(v bool) { t.fullPacketSize = v }

// SetReadTimeout sets a per-read deadline (0 disables).
func (t *Transport) SetReadTimeout(d time.Duration) { t.readTimeout = d }

// Conn returns the underlying connection.
func (t *Transport) Conn() net.Conn { return t.conn }

// Connected reports whether the transport is usable.
func (t *Transport) Connected() bool { return t != nil && !t.closed && t.conn != nil }

// Close closes the socket.
func (t *Transport) Close() error {
	if t == nil || t.closed || t.conn == nil {
		return nil
	}
	t.closed = true
	return t.conn.Close()
}

// OOBSupported reports whether urgent-data breaks can be sent on this
// transport (plain TCP on a Unix platform).
func (t *Transport) OOBSupported() bool {
	if !oobSupported() {
		return false
	}
	_, ok := t.conn.(*net.TCPConn)
	return ok
}

// SendOOB sends the out-of-band attention byte.
func (t *Transport) SendOOB() error {
	if !t.Connected() {
		return ErrConnectionClosed
	}
	if t.Trace != nil {
		t.Trace("send out-of-band break")
	}
	return sendOOB(t.conn)
}

// UpgradeTLS wraps the existing socket in TLS (used for TCPS renegotiation
// and for the initial TCPS handshake).
func (t *Transport) UpgradeTLS(cfg *tls.Config) error {
	tc := tls.Client(t.conn, cfg)
	if err := tc.Handshake(); err != nil {
		return fmt.Errorf("oracle: TLS handshake failed: %w", err)
	}
	t.conn = tc
	if t.Trace != nil {
		t.Trace("TLS negotiated: %s", tls.CipherSuiteName(tc.ConnectionState().CipherSuite))
	}
	return nil
}

// ReadPacket reads one complete packet. It returns ErrConnectionClosed on
// EOF or a broken socket.
func (t *Transport) ReadPacket() (*Packet, error) {
	if !t.Connected() {
		return nil, ErrConnectionClosed
	}
	if t.readTimeout > 0 {
		_ = t.conn.SetReadDeadline(time.Now().Add(t.readTimeout))
	} else {
		_ = t.conn.SetReadDeadline(time.Time{})
	}
	if _, err := io.ReadFull(t.conn, t.hdr[:]); err != nil {
		t.closed = true
		return nil, t.wrapErr(err)
	}
	var size int
	if t.fullPacketSize {
		size = int(binary.BigEndian.Uint32(t.hdr[0:4]))
	} else {
		size = int(binary.BigEndian.Uint16(t.hdr[0:2]))
	}
	if size < PacketHeaderSize || size > MaxSDU*4 {
		t.closed = true
		return nil, fmt.Errorf("oracle: invalid packet size %d", size)
	}
	buf := make([]byte, size)
	copy(buf, t.hdr[:])
	if _, err := io.ReadFull(t.conn, buf[PacketHeaderSize:]); err != nil {
		t.closed = true
		return nil, t.wrapErr(err)
	}
	if t.security.Active() && buf[4] == PacketTypeData && len(buf) > dataPayloadOffset+1 {
		plain, err := t.security.unwrap(buf[dataPayloadOffset:])
		if err != nil {
			t.closed = true
			return nil, err
		}
		nb := make([]byte, dataPayloadOffset+len(plain))
		copy(nb, buf[:dataPayloadOffset])
		copy(nb[dataPayloadOffset:], plain)
		buf = nb
	}
	p := &Packet{Type: buf[4], Flags: buf[5], Buf: buf}
	if t.Trace != nil {
		t.Trace("recv packet type=%d flags=%#x size=%d\n%s", p.Type, p.Flags, size, hexDump(buf))
	}
	return p, nil
}

// WritePacket sends raw packet bytes.
func (t *Transport) WritePacket(data []byte) error {
	if !t.Connected() {
		return ErrConnectionClosed
	}
	if t.Trace != nil {
		t.Trace("send packet type=%d size=%d\n%s", data[4], len(data), hexDump(data))
	}
	if t.security.Active() && data[4] == PacketTypeData && len(data) >= dataPayloadOffset {
		wrapped, err := t.security.wrap(data[dataPayloadOffset:])
		if err != nil {
			return err
		}
		nb := make([]byte, dataPayloadOffset+len(wrapped))
		copy(nb, data[:dataPayloadOffset])
		copy(nb[dataPayloadOffset:], wrapped)
		if t.fullPacketSize {
			binary.BigEndian.PutUint32(nb[0:], uint32(len(nb)))
		} else {
			binary.BigEndian.PutUint16(nb[0:], uint16(len(nb)))
		}
		data = nb
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_ = t.conn.SetWriteDeadline(time.Time{})
	if _, err := t.conn.Write(data); err != nil {
		t.closed = true
		return t.wrapErr(err)
	}
	return nil
}

func (t *Transport) wrapErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrConnectionClosed
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("oracle: socket timeout: %w", err)
	}
	return fmt.Errorf("%w: %v", ErrConnectionClosed, err)
}

func hexDump(b []byte) string {
	var sb strings.Builder
	for off := 0; off < len(b); off += 16 {
		end := off + 16
		if end > len(b) {
			end = len(b)
		}
		fmt.Fprintf(&sb, "%04x : ", off)
		for i := off; i < off+16; i++ {
			if i < end {
				fmt.Fprintf(&sb, "%02X ", b[i])
			} else {
				sb.WriteString("   ")
			}
		}
		sb.WriteString("|")
		for i := off; i < end; i++ {
			c := b[i]
			if c >= 32 && c < 127 {
				sb.WriteByte(c)
			} else {
				sb.WriteByte('.')
			}
		}
		sb.WriteString("|\n")
	}
	return sb.String()
}
