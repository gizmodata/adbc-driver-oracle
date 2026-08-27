package ttc

import (
	"context"
	"encoding/binary"
	"errors"
	"unicode/utf16"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

// LOB operations and locator flags.
const (
	lobOpRead = 0x0002

	lobLocOffsetFlag3        = 6
	lobLocOffsetFlag4        = 7
	lobLocFlagsVarLenCharset = 0x80
	lobLocFlagsLittleEndian  = 0x40
	lobMaxAmount             = 1<<32 - 1
)

// lobOpMessage performs a LOB operation (function 96); only READ is used.
type lobOpMessage struct {
	baseMessage
	operation    uint32
	locator      []byte
	sourceOffset uint64
	amount       int64
	sendAmount   bool
	data         []byte
	gotData      bool
}

func (m *lobOpMessage) write(w *tns.WriteBuffer) {
	m.writeFunctionCode(w)
	w.WriteUint8(1) // source pointer
	w.WriteUB4(uint32(len(m.locator)))
	w.WriteUint8(0) // dest pointer
	w.WriteUB4(0)   // dest length
	w.WriteUB4(0)   // short source offset
	w.WriteUB4(0)   // short dest offset
	w.WriteUint8(0) // pointer (character set)
	w.WriteUint8(0) // pointer (short amount)
	w.WriteUint8(0) // pointer (NULL LOB)
	w.WriteUB4(m.operation)
	w.WriteUint8(0) // pointer (SCN array)
	w.WriteUint8(0) // SCN array length
	w.WriteUB8(m.sourceOffset)
	w.WriteUB8(0) // dest offset
	if m.sendAmount {
		w.WriteUint8(1)
	} else {
		w.WriteUint8(0)
	}
	for i := 0; i < 3; i++ {
		w.WriteUint16BE(0)
	}
	w.WriteBytes(m.locator)
	if m.sendAmount {
		w.WriteUB8(uint64(m.amount))
	}
}

func (m *lobOpMessage) processMessage(r *tns.ReadBuffer, msgType uint8) error {
	if msgType == tns.MsgTypeLOBData {
		b := r.ReadRawBytesAndLength()
		if r.Err() != nil {
			return r.Err()
		}
		m.data = append(m.data, b...)
		m.gotData = true
		return nil
	}
	return m.baseMessage.processMessage(r, msgType)
}

func (m *lobOpMessage) processReturnParameters(r *tns.ReadBuffer) error {
	loc := r.ReadRawBytes(len(m.locator))
	if loc != nil {
		m.locator = append(m.locator[:0], loc...)
	}
	if m.sendAmount {
		m.amount = r.ReadSB8()
	}
	return r.Err()
}

// ReadLOB reads the entire contents of a LOB given its locator. CLOB /
// NCLOB contents are returned transcoded to UTF-8.
func (c *Conn) ReadLOB(ctx context.Context, locator []byte, isCharacter, nchar bool) ([]byte, error) {
	if len(locator) == 0 {
		return nil, errors.New("oracle: empty LOB locator")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := &lobOpMessage{operation: lobOpRead, locator: append([]byte(nil), locator...), sourceOffset: 1, amount: lobMaxAmount, sendAmount: true}
	m.init(c, funcLOBOp)
	if err := c.processMessageCtx(ctx, m); err != nil {
		return nil, err
	}
	data := m.data
	if !isCharacter {
		return data, nil
	}
	if nchar || (len(locator) > lobLocOffsetFlag3 && locator[lobLocOffsetFlag3]&lobLocFlagsVarLenCharset != 0) {
		if len(locator) > lobLocOffsetFlag4 && !nchar && locator[lobLocOffsetFlag4]&lobLocFlagsLittleEndian != 0 {
			return []byte(decodeUTF16LE(data)), nil
		}
		return []byte(tns.DecodeUTF16BE(data)), nil
	}
	return data, nil
}

func decodeUTF16LE(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}
