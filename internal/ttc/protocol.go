package ttc

import (
	"fmt"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

// driverName is what we report to the server as the client driver.
const driverName = "adbc-driver-oracle"

// protocolMessage negotiates the TTC protocol version and capabilities.
type protocolMessage struct {
	baseMessage
	serverVersion     uint8
	serverFlags       uint8
	serverBanner      string
	serverCompileCaps []byte
	serverRuntimeCaps []byte
}

func (m *protocolMessage) write(w *tns.WriteBuffer) {
	w.WriteUint8(tns.MsgTypeProtocol)
	w.WriteUint8(6) // protocol version (8.1 and higher)
	w.WriteUint8(0) // "array" terminator
	w.WriteStr(driverName)
	w.WriteUint8(0) // NULL terminator
}

func (m *protocolMessage) processMessage(r *tns.ReadBuffer, msgType uint8) error {
	if msgType != tns.MsgTypeProtocol {
		return m.baseMessage.processMessage(r, msgType)
	}
	if err := m.processProtocolInfo(r); err != nil {
		return err
	}
	if !r.Caps().SupportsEndOfResponse {
		m.endOfResponse = true
	}
	return nil
}

func (m *protocolMessage) processProtocolInfo(r *tns.ReadBuffer) error {
	caps := r.Caps()
	m.serverVersion = r.ReadUB1()
	r.SkipUB1()
	m.serverBanner = string(r.ReadNullTerminatedBytes())
	caps.CharsetID = r.ReadUint16LE()
	m.serverFlags = r.ReadUB1()
	numElem := r.ReadUint16LE()
	if numElem > 0 {
		r.SkipRawBytes(int(numElem) * 5)
	}
	fdoLen := r.ReadUint16BE()
	fdo := r.ReadRawBytes(int(fdoLen))
	if r.Err() != nil {
		return r.Err()
	}
	if len(fdo) < 7 {
		return fmt.Errorf("oracle: protocol FDO too short")
	}
	ix := 6 + int(fdo[5]) + int(fdo[6])
	if len(fdo) < ix+5 {
		return fmt.Errorf("oracle: protocol FDO too short for ncharset")
	}
	caps.NCharsetID = uint16(fdo[ix+3])<<8 | uint16(fdo[ix+4])
	m.serverCompileCaps = r.ReadBytes()
	if m.serverCompileCaps != nil {
		caps.AdjustForServerCompileCaps(m.serverCompileCaps)
	}
	m.serverRuntimeCaps = r.ReadBytes()
	if m.serverRuntimeCaps != nil {
		caps.AdjustForServerRuntimeCaps(m.serverRuntimeCaps)
	}
	return r.Err()
}

// dataTypesMessage negotiates the data type representations.
type dataTypesMessage struct{ baseMessage }

func (m *dataTypesMessage) write(w *tns.WriteBuffer) {
	w.WriteUint8(tns.MsgTypeDataTypes)
	w.WriteUint16LE(tns.CharsetUTF8)
	w.WriteUint16LE(tns.CharsetUTF8)
	w.WriteUint8(tns.EncodingMultiByte | tns.EncodingConvLength)
	w.WriteBytesWithLength(w.Caps().CompileCaps)
	w.WriteBytesWithLength(w.Caps().RuntimeCaps)
	for _, dt := range dataTypes {
		w.WriteUint16BE(dt[0])
		w.WriteUint16BE(dt[1])
		w.WriteUint16BE(dt[2])
		w.WriteUint16BE(0)
	}
	w.WriteUint16BE(0)
}

func (m *dataTypesMessage) processMessage(r *tns.ReadBuffer, msgType uint8) error {
	if msgType != tns.MsgTypeDataTypes {
		return m.baseMessage.processMessage(r, msgType)
	}
	for {
		dataType := r.ReadUint16BE()
		if dataType == 0 || r.Err() != nil {
			break
		}
		conv := r.ReadUint16BE()
		if conv != 0 {
			r.SkipRawBytes(4)
		}
	}
	if !r.Caps().SupportsEndOfResponse {
		m.endOfResponse = true
	}
	return r.Err()
}

// fastAuthMessage bundles protocol, data types and auth into one round
// trip (Oracle Database 23ai+).
type fastAuthMessage struct {
	baseMessage
	protocol  *protocolMessage
	dataTypes *dataTypesMessage
	auth      *authMessage
}

func (m *fastAuthMessage) write(w *tns.WriteBuffer) {
	w.WriteUint8(tns.MsgTypeFastAuth)
	w.WriteUint8(1) // fast auth version
	w.WriteUint8(1) // flag 1: server converts chars
	w.WriteUint8(0) // flag 2
	m.protocol.write(w)
	w.WriteUint16BE(0) // server charset (unused)
	w.WriteUint8(0)    // server charset flag (unused)
	w.WriteUint16BE(0) // server ncharset (unused)
	caps := w.Caps()
	caps.TTCFieldVersion = tns.FieldVersion19_1Ext1
	w.WriteUint8(caps.TTCFieldVersion)
	m.dataTypes.write(w)
	m.auth.write(w)
	caps.TTCFieldVersion = tns.FieldVersionMax
}

func (m *fastAuthMessage) processMessage(r *tns.ReadBuffer, msgType uint8) error {
	switch msgType {
	case tns.MsgTypeProtocol:
		return m.protocol.processMessage(r, msgType)
	case tns.MsgTypeDataTypes:
		return m.dataTypes.processMessage(r, msgType)
	default:
		m.auth.self = m.auth
		err := m.auth.processMessage(r, msgType)
		m.endOfResponse = m.auth.endOfResponse
		if m.auth.errorOccurred {
			m.errorOccurred = true
			m.errInfo = m.auth.errInfo
		}
		return err
	}
}
