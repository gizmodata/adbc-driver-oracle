package tns

import "encoding/binary"

// Packet is one TNS packet as read from the transport. Buf holds the full
// packet including its 8-byte header.
type Packet struct {
	Type  uint8
	Flags uint8
	Buf   []byte
}

// Size returns the packet size including the header.
func (p *Packet) Size() int { return len(p.Buf) }

// HasEndOfResponse reports whether the DATA packet carries the end-of-
// response flag or consists solely of an end-of-response message.
func (p *Packet) HasEndOfResponse() bool {
	if len(p.Buf) < PacketHeaderSize+2 {
		return false
	}
	flags := binary.BigEndian.Uint16(p.Buf[PacketHeaderSize:])
	if flags&DataFlagsEndOfResponse != 0 || flags&DataFlagsEOF != 0 {
		return true
	}
	if len(p.Buf) == PacketHeaderSize+3 && p.Buf[PacketHeaderSize+2] == MsgTypeEndOfResponse {
		return true
	}
	return false
}
