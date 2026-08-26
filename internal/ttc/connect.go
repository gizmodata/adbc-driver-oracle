package ttc

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

// connectMessage sends the TNS CONNECT packet and processes ACCEPT,
// REDIRECT, RESEND and REFUSE responses.
type connectMessage struct {
	baseMessage
	connectString []byte
	packetFlags   uint8
	sdu           uint32
	host          string
	port          int
	serviceName   string
	sid           string

	redirectData        string
	redirectDataLen     uint16
	readRedirectDataLen bool
	refuseMessage       string
	accepted            bool
}

func (m *connectMessage) write(w *tns.WriteBuffer) {
	serviceOptions := tns.GSODontCare
	var connectFlags1, connectFlags2 uint32
	nsiFlags := tns.NSISupportSecurityRen | tns.NSIDisableNA
	if w.Caps().SupportsOOB {
		serviceOptions |= tns.GSOCanRecvAttention
		connectFlags2 |= tns.CheckOOB
	}
	sdu16 := m.sdu
	if sdu16 > 0xffff {
		sdu16 = 0xffff
	}
	w.StartRequest(tns.PacketTypeConnect, m.packetFlags, 0)
	w.WriteUint16BE(tns.VersionDesired)
	w.WriteUint16BE(tns.VersionMinimum)
	w.WriteUint16BE(serviceOptions)
	w.WriteUint16BE(uint16(sdu16))
	w.WriteUint16BE(uint16(sdu16))
	w.WriteUint16BE(tns.ProtocolCharacteristics)
	w.WriteUint16BE(0) // line turnaround
	w.WriteUint16BE(1) // value of 1
	w.WriteUint16BE(uint16(len(m.connectString)))
	w.WriteUint16BE(74) // offset to connect data
	w.WriteUint32BE(0)  // max receivable data
	w.WriteUint8(nsiFlags)
	w.WriteUint8(nsiFlags)
	w.WriteUint64BE(0) // obsolete
	w.WriteUint64BE(0)
	w.WriteUint64BE(0)
	w.WriteUint32BE(m.sdu) // SDU (large)
	w.WriteUint32BE(m.sdu) // TDU (large)
	w.WriteUint32BE(connectFlags1)
	w.WriteUint32BE(connectFlags2)
	if len(m.connectString) > tns.MaxConnectData {
		_ = w.EndRequest()
		w.StartRequest(tns.PacketTypeData, 0, 0)
	}
	w.WriteBytes(m.connectString)
}

// processPacket handles the response packet (not a TTC message stream).
func (m *connectMessage) processPacket(r *tns.ReadBuffer) error {
	p := r.CurrentPacket()
	switch p.Type {
	case tns.PacketTypeRedirect:
		if !m.readRedirectDataLen {
			m.redirectDataLen = r.ReadUint16BE()
			m.readRedirectDataLen = true
		}
		if err := r.WaitForPackets(false); err != nil {
			return err
		}
		data := r.ReadRawBytes(int(m.redirectDataLen))
		if m.redirectDataLen > 0 {
			m.redirectData = string(data)
		}
		m.readRedirectDataLen = false
	case tns.PacketTypeAccept:
		protocolVersion := r.ReadUint16BE()
		if protocolVersion < tns.VersionMinAccepted {
			return fmt.Errorf("oracle: server protocol version %d not supported (12.1+ required)", protocolVersion)
		}
		protocolOptions := r.ReadUint16BE()
		r.SkipRawBytes(10)
		flags1 := r.ReadUB1()
		if flags1&tns.NSINARequired != 0 {
			return fmt.Errorf("oracle: Native Network Encryption and Data Integrity is not supported by this driver; configure the server with SQLNET.ENCRYPTION_SERVER=rejected/accepted or use TLS (tcps)")
		}
		r.SkipRawBytes(9)
		r.Caps().SDU = r.ReadUint32BE()
		var flags2 uint32
		if protocolVersion >= tns.VersionMinOOBCheck {
			r.SkipRawBytes(5)
			flags2 = r.ReadUint32BE()
		}
		r.Caps().AdjustForProtocol(protocolVersion, protocolOptions, flags2)
		r.Transport().SetFullPacketSize(true)
		m.accepted = true
	case tns.PacketTypeRefuse:
		code := 0
		if pos := strings.Index(m.refuseMessage, "(ERR="); pos >= 0 {
			if end := strings.Index(m.refuseMessage[pos:], ")"); end > 0 {
				code, _ = strconv.Atoi(m.refuseMessage[pos+5 : pos+end])
			}
		}
		switch code {
		case 0:
			return fmt.Errorf("oracle: listener refused connection: %s", m.refuseMessage)
		case tns.ErrInvalidServiceName:
			return &Error{Code: code, Message: fmt.Sprintf("ORA-12514: listener at %s:%d does not currently know of service %q", m.host, m.port, m.serviceName)}
		case tns.ErrInvalidSID:
			return &Error{Code: code, Message: fmt.Sprintf("ORA-12505: listener at %s:%d does not currently know of SID %q", m.host, m.port, m.sid)}
		default:
			return &Error{Code: code, Message: fmt.Sprintf("ORA-%05d: listener refused connection: %s", code, m.refuseMessage)}
		}
	}
	return r.Err()
}

func (m *connectMessage) processMessage(r *tns.ReadBuffer, msgType uint8) error {
	return fmt.Errorf("oracle: unexpected TTC message during connect")
}
