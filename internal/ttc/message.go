package ttc

import (
	"fmt"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

// TTC function codes.
const (
	funcAuthPhaseOne      = 118
	funcAuthPhaseTwo      = 115
	funcCloseCursors      = 105
	funcCommit            = 14
	funcExecute           = 94
	funcFetch             = 5
	funcLOBOp             = 96
	funcLogoff            = 9
	funcPing              = 147
	funcReexecute         = 4
	funcReexecuteAndFetch = 78
	funcRollback          = 15
	funcSessionState      = 176
	funcSetEndToEndAttr   = 135
	funcSetSchema         = 152
)

// Server-side piggyback op codes.
const (
	piggybackQueryCacheInvalidation = 1
	piggybackOSPIDMTS               = 2
	piggybackTraceEvent             = 3
	piggybackSessRet                = 4
	piggybackSync                   = 5
	piggybackLTXID                  = 7
	piggybackACReplayContext        = 8
	piggybackExtSync                = 9
	piggybackSessSignature          = 10
)

const (
	keywordNumCurrentSchema = 168
	keywordNumEdition       = 172
	keywordNumTransactionID = 201

	eocsFlagsTxnInProgress = 0x00000002
	eocsFlagsSessRelease   = 0x00008000
	sessgetSessionChanged  = 4
)

// message is one request/response exchange with the server.
type message interface {
	// write encodes the request.
	write(w *tns.WriteBuffer)
	// processMessage handles one server message of the given type.
	processMessage(r *tns.ReadBuffer, msgType uint8) error
	base() *baseMessage
}

// baseMessage carries state common to every message exchange.
type baseMessage struct {
	self          message // the outer message, for overridable hooks
	conn          *Conn
	functionCode  uint8
	messageType   uint8
	callStatus    uint32
	endOfResponse bool
	errorOccurred bool
	flushOutBinds bool
	resend        bool
	retry         bool
	errInfo       Error
	warning       *Error
	errRowid      tns.Rowid
	errCursorID   uint16
	batchErrors   []Error
}

func (m *baseMessage) base() *baseMessage { return m }

func (m *baseMessage) init(c *Conn, functionCode uint8) {
	m.conn = c
	m.functionCode = functionCode
	m.messageType = tns.MsgTypeFunction
}

// write writes just the function code header (used by simple messages).
func (m *baseMessage) write(w *tns.WriteBuffer) {
	m.writeFunctionCode(w)
}

func (m *baseMessage) writeFunctionCode(w *tns.WriteBuffer) {
	m.writePiggybacks(w)
	w.WriteUint8(m.messageType)
	w.WriteUint8(m.functionCode)
	w.WriteSeqNum()
	if w.Caps().TTCFieldVersion >= tns.FieldVersion23_1Ext1 {
		w.WriteUB8(0) // token number
	}
}

func (m *baseMessage) writePiggybackCode(w *tns.WriteBuffer, code uint8) {
	w.WriteUint8(tns.MsgTypePiggyback)
	w.WriteUint8(code)
	w.WriteSeqNum()
	if w.Caps().TTCFieldVersion >= tns.FieldVersion23_1Ext1 {
		w.WriteUB8(0)
	}
}

func (m *baseMessage) writePiggybacks(w *tns.WriteBuffer) {
	c := m.conn
	if c.currentSchemaModified {
		m.writePiggybackCode(w, funcSetSchema)
		w.WriteUint8(1)
		w.WriteBytesWithTwoLengths([]byte(c.currentSchema))
		c.currentSchemaModified = false
	}
	if len(c.cursorsToClose) > 0 {
		m.writePiggybackCode(w, funcCloseCursors)
		w.WriteUint8(1)
		w.WriteUB4(uint32(len(c.cursorsToClose)))
		for _, id := range c.cursorsToClose {
			w.WriteUB4(uint32(id))
		}
		c.cursorsToClose = c.cursorsToClose[:0]
	}
	if c.endToEndModified {
		m.writeEndToEndPiggyback(w)
	}
}

func (m *baseMessage) writeEndToEndPiggyback(w *tns.WriteBuffer) {
	c := m.conn
	const (
		flagAction           = 0x0010
		flagClientIdentifier = 0x0001
		flagClientInfo       = 0x0100
		flagModule           = 0x0008
	)
	var flags uint32
	if c.actionModified {
		flags |= flagAction
	}
	if c.clientIdentifierModified {
		flags |= flagClientIdentifier
	}
	if c.clientInfoModified {
		flags |= flagClientInfo
	}
	if c.moduleModified {
		flags |= flagModule
	}
	m.writePiggybackCode(w, funcSetEndToEndAttr)
	w.WriteUint8(0) // pointer (cidnam)
	w.WriteUint8(0) // pointer (cidser)
	w.WriteUB4(flags)
	writeHdr := func(modified bool, v string) {
		if modified {
			w.WriteUint8(1)
			w.WriteUB4(uint32(len(v)))
		} else {
			w.WriteUint8(0)
			w.WriteUB4(0)
		}
	}
	writeHdr(c.clientIdentifierModified, c.clientIdentifier)
	writeHdr(c.moduleModified, c.module)
	writeHdr(c.actionModified, c.action)
	w.WriteUint8(0) // pointer (cideci)
	w.WriteUB4(0)
	w.WriteUint8(0) // cidcct
	w.WriteUB4(0)   // cidecs
	writeHdr(c.clientInfoModified, c.clientInfo)
	w.WriteUint8(0) // cidkstk
	w.WriteUB4(0)
	w.WriteUint8(0) // cidktgt
	w.WriteUB4(0)
	w.WriteUint8(0) // dbop
	w.WriteUB4(0)
	if c.clientIdentifierModified && c.clientIdentifier != "" {
		w.WriteBytesWithLength([]byte(c.clientIdentifier))
	}
	if c.moduleModified && c.module != "" {
		w.WriteBytesWithLength([]byte(c.module))
	}
	if c.actionModified && c.action != "" {
		w.WriteBytesWithLength([]byte(c.action))
	}
	if c.clientInfoModified && c.clientInfo != "" {
		w.WriteBytesWithLength([]byte(c.clientInfo))
	}
	c.actionModified, c.clientIdentifierModified, c.clientInfoModified, c.moduleModified = false, false, false, false
	c.endToEndModified = false
}

// processMessage handles the message types common to every exchange.
func (m *baseMessage) processMessage(r *tns.ReadBuffer, msgType uint8) error {
	switch msgType {
	case tns.MsgTypeError:
		return m.processErrorInfo(r)
	case tns.MsgTypeWarning:
		m.processWarningInfo(r)
	case tns.MsgTypeToken:
		token := r.ReadUB8()
		if token != 0 {
			return fmt.Errorf("oracle: mismatched token %d (expected 0)", token)
		}
	case tns.MsgTypeStatus:
		m.callStatus = r.ReadUB4()
		r.ReadUB2() // end-to-end seq num
		if !r.Caps().SupportsEndOfResponse {
			m.endOfResponse = true
		}
	case tns.MsgTypeParameter:
		if p, ok := m.self.(returnParamsProcessor); ok {
			return p.processReturnParameters(r)
		}
		return m.processReturnParameters(r)
	case tns.MsgTypeServerSidePiggyback:
		return m.processServerSidePiggyback(r)
	case tns.MsgTypeEndOfResponse:
		m.endOfResponse = true
	default:
		return fmt.Errorf("oracle: unknown message type %d at position %d", msgType, r.Pos()-1)
	}
	return r.Err()
}

func (m *baseMessage) processReturnParameters(r *tns.ReadBuffer) error {
	return fmt.Errorf("oracle: unexpected parameter message for function %d", m.functionCode)
}

func (m *baseMessage) processErrorInfo(r *tns.ReadBuffer) error {
	info := &m.errInfo
	m.callStatus = r.ReadUB4() // end of call status
	r.SkipUB2()                // end to end seq#
	r.SkipUB4()                // current row number
	r.SkipUB2()                // error number
	r.SkipUB2()                // array elem error
	r.SkipUB2()                // array elem error
	m.errCursorID = r.ReadUB2()
	errorPos := r.ReadSB2()
	r.SkipUB1() // sql type (19c and earlier)
	r.SkipUB1() // fatal?
	r.SkipUB1() // flags
	r.SkipUB1() // user cursor options
	r.SkipUB1() // UPI parameter
	flags := r.ReadUB1()
	if flags&0x20 != 0 {
		m.warning = &Error{Code: 0, Message: "compilation error", IsWarning: true}
	}
	m.errRowid = r.ReadRowid()
	r.SkipUB4()             // OS error
	r.SkipUB1()             // statement number
	r.SkipUB1()             // call number
	r.SkipUB2()             // padding
	r.SkipUB4()             // success iters
	r.SkipBytesWithLength() // oerrdd (logical rowid)

	// batch error codes
	numErrors := r.ReadUB2()
	if numErrors > 0 {
		m.batchErrors = make([]Error, numErrors)
		first := r.ReadUB1()
		for i := 0; i < int(numErrors); i++ {
			if first == tns.LongLengthIndicator {
				r.SkipUB4()
			}
			m.batchErrors[i].Code = int(r.ReadUB2())
		}
		if first == tns.LongLengthIndicator {
			r.SkipRawBytes(1)
		}
	}
	numOffsets := r.ReadUB4()
	if numOffsets > 0 {
		if numOffsets > 65535 {
			return fmt.Errorf("oracle: too many batch errors (%d)", numOffsets)
		}
		first := r.ReadUB1()
		for i := 0; i < int(numOffsets); i++ {
			if first == tns.LongLengthIndicator {
				r.SkipUB4()
			}
			off := r.ReadUB4()
			if i < int(numErrors) {
				m.batchErrors[i].Offset = int(off)
			}
		}
		if first == tns.LongLengthIndicator {
			r.SkipRawBytes(1)
		}
	}
	numMsgs := r.ReadUB2()
	if numMsgs > 0 {
		r.SkipRawBytes(1)
		for i := 0; i < int(numMsgs); i++ {
			r.SkipUB2()
			s, _ := r.ReadStr(false)
			if i < len(m.batchErrors) {
				m.batchErrors[i].Message = s
			}
			r.SkipRawBytes(2)
		}
	}
	info.Code = int(r.ReadUB4())
	info.RowCount = r.ReadUB8()
	if r.Caps().TTCFieldVersion >= tns.FieldVersion20_1 {
		r.SkipUB4() // sql type
		r.SkipUB4() // server checksum
	}
	if info.Code != 0 {
		m.errorOccurred = true
		if errorPos > 0 {
			info.Offset = int(errorPos)
		}
		info.Message, _ = r.ReadStr(false)
	}
	if !r.Caps().SupportsEndOfResponse {
		m.endOfResponse = true
	}
	return r.Err()
}

func (m *baseMessage) processWarningInfo(r *tns.ReadBuffer) {
	errNum := r.ReadUB2()
	numBytes := r.ReadUB2()
	r.SkipUB2() // flags
	if errNum != 0 && numBytes > 0 {
		msg, _ := r.ReadStr(false)
		m.warning = &Error{Code: int(errNum), Message: msg, IsWarning: true}
	}
}

func (m *baseMessage) processKeywordValuePairs(r *tns.ReadBuffer, numPairs uint16) {
	for i := 0; i < int(numPairs); i++ {
		var text string
		if n := r.ReadUB2(); n > 0 {
			text, _ = r.ReadStr(false)
		}
		if n := r.ReadUB2(); n > 0 {
			r.SkipBytes()
		}
		keyword := r.ReadUB2()
		switch keyword {
		case keywordNumCurrentSchema:
			m.conn.currentSchema = text
		case keywordNumEdition:
			m.conn.edition = text
		}
	}
}

func (m *baseMessage) processServerSidePiggyback(r *tns.ReadBuffer) error {
	opcode := r.ReadUB1()
	switch opcode {
	case piggybackLTXID:
		r.ReadBytesWithLength()
	case piggybackQueryCacheInvalidation, piggybackTraceEvent:
	case piggybackOSPIDMTS:
		r.ReadUB2()
		r.SkipBytes()
	case piggybackSync:
		r.SkipUB2()
		r.SkipUB1()
		n := r.ReadUB2()
		r.SkipUB1()
		m.processKeywordValuePairs(r, n)
		r.SkipUB4()
	case piggybackExtSync:
		r.SkipUB2()
		r.SkipUB1()
	case piggybackACReplayContext:
		r.SkipUB2()
		r.SkipUB1()
		r.SkipUB4()
		r.SkipUB4()
		r.SkipUB1()
		r.SkipBytesWithLength()
	case piggybackSessRet:
		r.SkipUB2()
		r.SkipUB1()
		n := r.ReadUB2()
		if n > 0 {
			r.SkipUB1()
			for i := 0; i < int(n); i++ {
				if r.ReadUB2() > 0 {
					r.SkipBytes()
				}
				if r.ReadUB2() > 0 {
					r.SkipBytes()
				}
				r.SkipUB2()
			}
		}
		flags := r.ReadUB4()
		if flags&sessgetSessionChanged != 0 {
			m.conn.cursorsToClose = m.conn.cursorsToClose[:0]
		}
		m.conn.sessionID = r.ReadUB4()
		m.conn.serialNum = r.ReadUB2()
	case piggybackSessSignature:
		r.SkipUB2()
		r.SkipUB1()
		r.SkipUB8()
		r.SkipUB8()
		r.SkipUB8()
	default:
		return fmt.Errorf("oracle: unknown server piggyback opcode %d", opcode)
	}
	return r.Err()
}

// returnParamsProcessor is implemented by messages that expect a
// parameter (return values) message.
type returnParamsProcessor interface {
	processReturnParameters(r *tns.ReadBuffer) error
}

// process drives the response loop for a message.
func process(m message, r *tns.ReadBuffer) error {
	b := m.base()
	if b.self == nil {
		b.self = m
	}
	b.endOfResponse = false
	b.flushOutBinds = false
	for !b.endOfResponse {
		msgType := r.ReadUB1()
		if err := r.Err(); err != nil {
			return err
		}
		if err := m.processMessage(r, msgType); err != nil {
			return err
		}
		if err := r.Err(); err != nil {
			return err
		}
	}
	return nil
}

// simpleMessage is a function-code-only request (commit, rollback, ...).
type simpleMessage struct{ baseMessage }

func newSimpleMessage(c *Conn, fn uint8) *simpleMessage {
	m := &simpleMessage{}
	m.init(c, fn)
	return m
}
