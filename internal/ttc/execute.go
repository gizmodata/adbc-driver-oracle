package ttc

import (
	"context"
	"errors"
	"fmt"

	"github.com/gizmodata/adbc-driver-oracle/internal/oratype"
	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

// Execute options and flags.
const (
	execOptionParse             = 0x01
	execOptionBind              = 0x08
	execOptionDefine            = 0x10
	execOptionExecute           = 0x20
	execOptionFetch             = 0x40
	execOptionCommit            = 0x100
	execOptionPLSQLBind         = 0x400
	execOptionNotPLSQL          = 0x8000
	execOptionDescribe          = 0x20000
	execOptionNoCompressedFetch = 0x40000
	execOptionBatchErrors       = 0x80000

	execFlagsDMLRowCounts      = 0x4000
	execFlagsImplicitResultset = 0x8000

	bindUseIndicators = 0x0001
	bindArray         = 0x0040

	bindDirOutput      = 16
	bindDirInput       = 32
	bindDirInputOutput = 48

	maxLongLength   = 0x7fffffff
	maxURowidLength = 5267
	lobPrefetchFlag = 0x2000000
)

// RowSink receives decoded row data during a fetch. Column values are
// delivered as their raw wire encoding (see Column.FetchType); the slice
// passed to AppendValue is only valid for the duration of the call.
type RowSink interface {
	AppendValue(col int, data []byte) error
	AppendNull(col int) error
	// AppendDuplicate repeats the previous row's value for the column.
	AppendDuplicate(col int) error
	FinishRow() error
}

// BindColumn describes one bind variable and yields its per-row values.
type BindColumn struct {
	OraTypeNum uint8
	CSForm     uint8
	BufferSize uint32
	// Value returns the encoded payload for the row (nil means NULL).
	Value func(row int) ([]byte, error)
}

// Statement is a prepared SQL statement bound to a connection.
type Statement struct {
	conn        *Conn
	sql         string
	sqlBytes    []byte
	kind        StatementKind
	bindNames   []string
	isReturning bool

	cursorID       uint16
	executed       bool
	requiresDefine bool
	noPrefetch     bool
	columns        []Column
	moreRows       bool
	rowCount       uint64
	lastRowid      string
	closed         bool
	warning        *Error
}

// Prepare parses SQL and returns a statement. No round trip happens.
func (c *Conn) Prepare(sql string) *Statement {
	p := parseSQL(sql)
	return &Statement{
		conn:        c,
		sql:         sql,
		sqlBytes:    []byte(sql),
		kind:        p.kind,
		bindNames:   p.bindNames,
		isReturning: p.isReturning,
	}
}

// Kind returns the statement classification.
func (s *Statement) Kind() StatementKind { return s.kind }

// IsQuery reports whether the statement produces a result set.
func (s *Statement) IsQuery() bool { return s.kind == KindQuery }

// BindNames returns the bind variable names in order of appearance.
func (s *Statement) BindNames() []string { return s.bindNames }

// Columns returns the result set columns (after Execute or Describe).
func (s *Statement) Columns() []Column { return s.columns }

// MoreRows reports whether further Fetch calls may return rows.
func (s *Statement) MoreRows() bool { return s.moreRows }

// RowCount returns the number of rows affected by DML.
func (s *Statement) RowCount() uint64 { return s.rowCount }

// Warning returns the last compilation warning, if any.
func (s *Statement) Warning() *Error { return s.warning }

// Close releases the server cursor (deferred to the next round trip).
func (s *Statement) Close() {
	if s.closed {
		return
	}
	s.closed = true
	s.conn.mu.Lock()
	s.conn.addCursorToClose(s.cursorID)
	s.conn.mu.Unlock()
	s.cursorID = 0
}

func (s *Statement) clearCursor() {
	s.conn.addCursorToClose(s.cursorID)
	s.cursorID = 0
	s.executed = false
	s.requiresDefine = false
	s.noPrefetch = false
}

// SetFetchTypes lets the caller override how columns are transferred
// (for example fetching JSON as text). It must be called after Describe
// and before the fetch pass; the statement re-defines columns as needed.
func (s *Statement) SetFetchTypes(fn func(c *Column)) {
	changed := false
	for i := range s.columns {
		before := s.columns[i].FetchType
		fn(&s.columns[i])
		if s.columns[i].FetchType != before {
			changed = true
		}
	}
	if changed {
		s.requiresDefine = true
		s.noPrefetch = true
	}
}

// defaultFetchTypes applies the standard conversions: LOBs are fetched
// inline as LONG / LONG RAW. JSON stays native (OSON, prefetched with
// the define pass) and is decoded by the caller.
func defaultFetchTypes(c *Column) {
	switch c.OraTypeNum {
	case TypeClob:
		c.FetchType = TypeLong
	case TypeBlob:
		c.FetchType = TypeLongRaw
	}
}

// ExecuteOptions controls Execute.
type ExecuteOptions struct {
	Binds        []BindColumn
	NumRows      int // number of bind rows (executemany); 0 or 1 for a single execution
	PrefetchRows int // rows to fetch with the execute for queries
	ArraySize    int // rows per subsequent fetch
	AutoCommit   bool
	ParseOnly    bool // describe without executing
	Sink         RowSink
	BatchErrors  bool
}

// Execute runs the statement. For queries, describe information is
// available afterwards via Columns() and the first PrefetchRows rows are
// delivered to the sink; use Fetch for the rest.
func (s *Statement) Execute(ctx context.Context, opts ExecuteOptions) error {
	if s.closed {
		return errors.New("oracle: statement is closed")
	}
	c := s.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Closed() {
		return tns.ErrConnectionClosed
	}
	numExecs := opts.NumRows
	if numExecs <= 0 {
		numExecs = 1
	}
	if len(opts.Binds) != len(s.bindNames) && !opts.ParseOnly {
		return fmt.Errorf("oracle: statement has %d bind variables but %d were supplied", len(s.bindNames), len(opts.Binds))
	}
	if opts.PrefetchRows <= 0 {
		opts.PrefetchRows = 1000
	}
	if opts.ArraySize <= 0 {
		opts.ArraySize = opts.PrefetchRows
	}
	if opts.NumRows > 1 && s.kind == KindQuery {
		return errors.New("oracle: cannot execute a query with multiple bind rows")
	}
	s.rowCount = 0
	s.warning = nil
	m := &executeMessage{}
	m.stmt = s
	m.binds = opts.Binds
	m.numExecs = numExecs
	m.parseOnly = opts.ParseOnly
	m.prefetchRows = opts.PrefetchRows
	m.arraySize = opts.ArraySize
	m.autoCommit = opts.AutoCommit
	m.useBatchErrors = opts.BatchErrors
	m.sink = opts.Sink
	m.init(c, funcExecute)
	if err := c.processMessage(m); err != nil {
		return err
	}
	if m.resend {
		// A define pass is required (LOB/JSON columns): resend with defines.
		m.resend = false
		m.rowIndex = 0
		if err := c.processMessage(m); err != nil {
			return err
		}
	}
	s.warning = m.warning
	return nil
}

// Describe performs a parse-only execute to obtain the column metadata.
func (s *Statement) Describe(ctx context.Context) error {
	return s.Execute(ctx, ExecuteOptions{ParseOnly: true})
}

// Fetch retrieves the next batch of rows into the sink.
func (s *Statement) Fetch(ctx context.Context, sink RowSink, arraySize int) error {
	if s.closed {
		return errors.New("oracle: statement is closed")
	}
	if !s.moreRows {
		return nil
	}
	if arraySize <= 0 {
		arraySize = 1000
	}
	c := s.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Closed() {
		return tns.ErrConnectionClosed
	}
	if s.cursorID == 0 {
		return errors.New("oracle: cursor has been closed")
	}
	m := &fetchMessage{}
	m.stmt = s
	m.sink = sink
	m.arraySize = arraySize
	m.inFetch = true
	m.init(c, funcFetch)
	return c.processMessage(m)
}

// messageWithData is the shared machinery for execute and fetch.
type messageWithData struct {
	baseMessage
	stmt           *Statement
	sink           RowSink
	binds          []BindColumn
	numExecs       int
	prefetchRows   int
	arraySize      int
	autoCommit     bool
	parseOnly      bool
	useBatchErrors bool
	inFetch        bool
	discardRows    bool
	rowIndex       int
	bitVector      []byte
	haveBitVector  bool
	numColumnsSent uint16
	outBindDirs    []uint8
}

func (m *messageWithData) processMessage(r *tns.ReadBuffer, msgType uint8) error {
	switch msgType {
	case tns.MsgTypeRowHeader:
		return m.processRowHeader(r)
	case tns.MsgTypeRowData:
		return m.processRowData(r)
	case tns.MsgTypeFlushOutBinds:
		m.flushOutBinds = true
		m.endOfResponse = true
		return nil
	case tns.MsgTypeDescribeInfo:
		r.SkipBytes()
		return m.processDescribeInfo(r)
	case tns.MsgTypeError:
		return m.processErrorInfo(r)
	case tns.MsgTypeBitVector:
		return m.processBitVector(r)
	case tns.MsgTypeIOVector:
		return m.processIOVector(r)
	case tns.MsgTypeImplicitResultset:
		return errors.New("oracle: implicit result sets are not supported")
	}
	return m.baseMessage.processMessage(r, msgType)
}

func (m *messageWithData) processReturnParameters(r *tns.ReadBuffer) error {
	numParams := r.ReadUB2()
	for i := 0; i < int(numParams); i++ {
		r.SkipUB4()
	}
	if n := r.ReadUB2(); n > 0 {
		r.SkipRawBytes(int(n))
	}
	numPairs := r.ReadUB2()
	m.processKeywordValuePairs(r, numPairs)
	if n := r.ReadUB2(); n > 0 {
		r.SkipRawBytes(int(n)) // registration info / query id
	}
	return r.Err()
}

func (m *messageWithData) processErrorInfo(r *tns.ReadBuffer) error {
	if err := m.baseMessage.processErrorInfo(r); err != nil {
		return err
	}
	s := m.stmt
	if m.errCursorID != 0 {
		s.cursorID = m.errCursorID
	}
	if s.kind != KindPLSQL && !m.inFetch {
		s.rowCount = m.errInfo.RowCount
	} else if m.inFetch && m.rowIndex > 0 {
		s.requiresDefine = false
	}
	if id := m.errRowid; id.RBA != 0 || id.BlockNum != 0 || id.SlotNum != 0 || id.PartitionID != 0 {
		s.lastRowid = oratype.EncodeRowid(id.RBA, id.PartitionID, id.BlockNum, id.SlotNum)
	}
	code := m.errInfo.Code
	switch {
	case code == tns.ErrNoDataFound && m.inFetch:
		m.errInfo.Code = 0
		s.moreRows = false
		s.requiresDefine = false
		m.errorOccurred = false
	case code == tns.ErrArrayDMLErrors:
		// Batch errors: surface the first one.
		if len(m.batchErrors) > 0 {
			be := m.batchErrors[0]
			m.errInfo = Error{Code: be.Code, Message: fmt.Sprintf("ORA-%05d: %s (row offset %d)", be.Code, be.Message, be.Offset), Offset: be.Offset}
		}
	case m.retry:
		m.retry = false
	case s.kind == KindQuery && (code == tns.ErrVarNotInSelectList || code == tns.ErrInconsistentDataTypes):
		m.retry = true
		s.clearCursor()
	case code != 0 && s.cursorID != 0:
		if !integrityErrorCodes[code] {
			s.clearCursor()
		}
	}
	return nil
}

func (m *messageWithData) processRowHeader(r *tns.ReadBuffer) error {
	r.SkipUB1() // flags
	r.SkipUB2() // num requests
	r.SkipUB4() // iteration number
	r.SkipUB4() // num iters
	r.SkipUB2() // buffer length
	numBytes := r.ReadUB4()
	if numBytes > 0 {
		r.SkipUB1() // repeated length
		m.getBitVector(r, int(numBytes))
	}
	r.SkipBytesWithLength() // rxhrid
	return r.Err()
}

func (m *messageWithData) processBitVector(r *tns.ReadBuffer) error {
	m.numColumnsSent = r.ReadUB2()
	numBytes := len(m.stmt.columns) / 8
	if len(m.stmt.columns)%8 > 0 {
		numBytes++
	}
	m.getBitVector(r, numBytes)
	return r.Err()
}

func (m *messageWithData) getBitVector(r *tns.ReadBuffer, numBytes int) {
	b := r.ReadRawBytes(numBytes)
	if b == nil {
		return
	}
	if cap(m.bitVector) < numBytes {
		m.bitVector = make([]byte, numBytes)
	}
	m.bitVector = m.bitVector[:numBytes]
	copy(m.bitVector, b)
	m.haveBitVector = true
}

func (m *messageWithData) isDuplicate(col int) bool {
	if !m.haveBitVector {
		return false
	}
	return m.bitVector[col/8]&(1<<(col%8)) == 0
}

func (m *messageWithData) processIOVector(r *tns.ReadBuffer) error {
	r.SkipUB1()                // flag
	numRequests := r.ReadUB2() // num requests
	numIters := r.ReadUB4()    // num iters
	numBinds := int(numIters)*256 + int(numRequests)
	r.SkipUB4() // num iters this time
	r.SkipUB2() // uac buffer length
	if n := r.ReadUB2(); n > 0 {
		r.SkipRawBytes(int(n))
	}
	if n := r.ReadUB2(); n > 0 {
		r.SkipRawBytes(int(n))
	}
	m.outBindDirs = make([]uint8, numBinds)
	for i := 0; i < numBinds; i++ {
		m.outBindDirs[i] = r.ReadUB1()
		if m.outBindDirs[i] != bindDirInput {
			return errors.New("oracle: OUT / IN OUT bind variables are not supported")
		}
	}
	return r.Err()
}

func (m *messageWithData) processDescribeInfo(r *tns.ReadBuffer) error {
	s := m.stmt
	r.SkipUB4() // max row size
	numColumns := r.ReadUB4()
	if numColumns > 0 {
		r.SkipUB1()
	}
	cols := make([]Column, numColumns)
	for i := range cols {
		cols[i] = processMetadata(r)
		if r.Err() != nil {
			return r.Err()
		}
		if cols[i].OraTypeNum == TypeObject && cols[i].ObjectTypeName == "XMLTYPE" {
			return fmt.Errorf("oracle: column %q: XMLTYPE columns are not supported; use XMLSERIALIZE or GETCLOBVAL()", cols[i].Name)
		}
		defaultFetchTypes(&cols[i])
		switch cols[i].OraTypeNum {
		case TypeBlob, TypeClob, TypeJSON, TypeVector:
			s.requiresDefine = true
			s.noPrefetch = true
		}
	}
	r.SkipBytesWithLength() // current date
	r.SkipUB4()             // dcbflag
	r.SkipUB4()             // dcbmdbz
	r.SkipUB4()             // dcbmnpr
	r.SkipUB4()             // dcbmxpr
	r.SkipBytesWithLength() // dcbqcky
	s.columns = cols
	if s.requiresDefine {
		// Rows returned by this pass carry LOB locators; discard them, the
		// define pass will re-fetch with the converted types.
		m.discardRows = true
	}
	return r.Err()
}

func (m *messageWithData) processRowData(r *tns.ReadBuffer) error {
	s := m.stmt
	deliver := m.sink != nil && !m.discardRows && m.inFetch
	for i := range s.columns {
		col := &s.columns[i]
		if m.inFetch && m.isDuplicate(i) {
			if deliver {
				if err := m.sink.AppendDuplicate(i); err != nil {
					return err
				}
			}
			continue
		}
		if err := m.processColumnData(r, i, col, deliver); err != nil {
			return err
		}
	}
	m.rowIndex++
	if m.inFetch {
		m.haveBitVector = false
	}
	if deliver {
		if err := m.sink.FinishRow(); err != nil {
			return err
		}
	}
	return r.Err()
}

func (m *messageWithData) processColumnData(r *tns.ReadBuffer, idx int, col *Column, deliver bool) error {
	typ := col.FetchType
	if col.BufferSize == 0 && m.inFetch && typ != TypeLong && typ != TypeLongRaw && typ != TypeURowid {
		// Column is NULL by describe: nothing is sent.
		if deliver {
			return m.sink.AppendNull(idx)
		}
		return nil
	}
	var data []byte
	isNull := false
	switch typ {
	case TypeRowid:
		n := r.ReadUB1()
		if n == 0 || n == tns.NullLengthIndicator {
			isNull = true
		} else {
			id := r.ReadRowid()
			data = []byte(oratype.EncodeRowid(id.RBA, id.PartitionID, id.BlockNum, id.SlotNum))
		}
	case TypeURowid:
		first := r.ReadRawBytesAndLength()
		if first == nil {
			isNull = true
		} else {
			raw := r.ReadRawBytesAndLength()
			data = []byte(oratype.DecodeURowid(raw))
		}
	case TypeCursor:
		return errors.New("oracle: REF CURSOR columns are not supported")
	case TypeClob, TypeBlob, TypeBfile:
		// LOB locator (only seen in the pre-define pass).
		n := r.ReadUB4()
		if n > 0 {
			if typ != TypeBfile {
				r.SkipUB8()
				r.SkipUB4()
			}
			r.SkipBytes()
		}
		isNull = true
	case TypeJSON, TypeVector:
		// Sent as a prefetched LOB: size, chunk size, data, locator.
		n := r.ReadUB4()
		if n == 0 {
			isNull = true
		} else {
			r.SkipUB8()
			r.SkipUB4()
			data = r.ReadBytes()
			r.SkipBytes()
			if data == nil {
				isNull = true
			}
		}
	case TypeObject:
		return fmt.Errorf("oracle: column %q: object type columns are not supported", col.Name)
	default:
		data = r.ReadRawBytesAndLength()
		if data == nil {
			isNull = true
		} else if col.FetchCSForm == CSFormNChar {
			switch typ {
			case TypeVarchar, TypeChar, TypeLong:
				data = []byte(tns.DecodeUTF16BE(data))
			}
		}
	}
	if err := r.Err(); err != nil {
		return err
	}
	if m.inFetch && (typ == TypeLong || typ == TypeLongRaw) {
		r.SkipSB4() // null indicator
		r.SkipUB4() // return code
	}
	if !deliver {
		return r.Err()
	}
	if isNull {
		return m.sink.AppendNull(idx)
	}
	return m.sink.AppendValue(idx, data)
}

// writeColumnMetadata writes bind/define metadata for a set of columns.
func writeColumnMetadata(w *tns.WriteBuffer, oraType, csForm uint8, bufferSize uint32) {
	if oraType == TypeRowid || oraType == TypeURowid {
		oraType = TypeVarchar
		bufferSize = maxURowidLength
	}
	var contFlag uint64
	var lobPrefetchLength uint32
	switch oraType {
	case TypeBlob, TypeClob:
		contFlag = lobPrefetchFlag
	case TypeJSON:
		contFlag = lobPrefetchFlag
		bufferSize = 32 * 1024 * 1024
		lobPrefetchLength = bufferSize
	case TypeVector:
		contFlag = lobPrefetchFlag
		bufferSize = 1024 * 1024
		lobPrefetchLength = bufferSize
	}
	w.WriteUint8(oraType)
	w.WriteUint8(bindUseIndicators)
	w.WriteUint8(0) // precision
	w.WriteUint8(0) // scale
	w.WriteUB4(bufferSize)
	w.WriteUB4(0) // max num elements
	w.WriteUB8(contFlag)
	w.WriteUB4(0) // OID
	w.WriteUB2(0) // version
	if csForm != 0 {
		w.WriteUB2(tns.CharsetUTF8)
	} else {
		w.WriteUB2(0)
	}
	w.WriteUint8(csForm)
	w.WriteUB4(lobPrefetchLength)
	if w.Caps().TTCFieldVersion >= tns.FieldVersion12_2 {
		w.WriteUB4(0) // oaccolid
	}
}

// executeMessage is the full execute (function 94).
type executeMessage struct {
	messageWithData
}

func (m *executeMessage) write(w *tns.WriteBuffer) {
	s := m.stmt
	if s.kind == KindQuery && !m.parseOnly {
		m.inFetch = true
		s.moreRows = true
		m.rowIndex = 0
		m.discardRows = false
	}
	var options, execFlags uint32
	numParams := 0
	numIters := uint32(1)
	if !s.requiresDefine && !m.parseOnly {
		numParams = len(m.binds)
	}
	if s.requiresDefine {
		options |= execOptionDefine
	} else if !m.parseOnly {
		execFlags |= execFlagsImplicitResultset
		options |= execOptionExecute
	}
	if s.cursorID == 0 || s.kind == KindDDL {
		options |= execOptionParse
	}
	if s.kind == KindQuery {
		if m.parseOnly {
			options |= execOptionDescribe
		} else {
			if s.cursorID == 0 || s.requiresDefine {
				numIters = uint32(m.prefetchRows)
			} else {
				numIters = uint32(m.arraySize)
			}
			if numIters > 0 && !s.noPrefetch {
				options |= execOptionFetch
			}
		}
	}
	if s.kind != KindPLSQL && !m.parseOnly {
		options |= execOptionNotPLSQL
	} else if s.kind == KindPLSQL && numParams > 0 {
		options |= execOptionPLSQLBind
	}
	if numParams > 0 {
		options |= execOptionBind
	}
	if m.useBatchErrors {
		options |= execOptionBatchErrors
	}
	if m.autoCommit && !m.parseOnly {
		options |= execOptionCommit
	}

	m.writeFunctionCode(w)
	w.WriteUB4(options)
	w.WriteUB4(uint32(s.cursorID))
	if s.cursorID == 0 || s.kind == KindDDL {
		w.WriteUint8(1)
		w.WriteUB4(uint32(len(s.sqlBytes)))
	} else {
		w.WriteUint8(0)
		w.WriteUB4(0)
	}
	w.WriteUint8(1) // pointer (vector)
	w.WriteUB4(13)  // al8i4 array length
	w.WriteUint8(0) // pointer (al8o4)
	w.WriteUint8(0) // pointer (al8o4l)
	w.WriteUB4(0)   // prefetch buffer size
	w.WriteUB4(numIters)
	w.WriteUB4(maxLongLength)
	if numParams == 0 {
		w.WriteUint8(0)
		w.WriteUB4(0)
	} else {
		w.WriteUint8(1)
		w.WriteUB4(uint32(numParams))
	}
	w.WriteUint8(0) // al8app
	w.WriteUint8(0) // al8txn
	w.WriteUint8(0) // al8txl
	w.WriteUint8(0) // al8kv
	w.WriteUint8(0) // al8kvl
	if s.requiresDefine {
		w.WriteUint8(1)
		w.WriteUB4(uint32(len(s.columns)))
	} else {
		w.WriteUint8(0)
		w.WriteUB4(0)
	}
	w.WriteUB4(0)   // registration id lsb
	w.WriteUint8(0) // al8objlist
	w.WriteUint8(1) // al8objlen
	w.WriteUint8(0) // al8blv
	w.WriteUB4(0)   // al8blvl
	w.WriteUint8(0) // al8dnam
	w.WriteUB4(0)   // al8dnaml
	w.WriteUB4(0)   // registration id msb
	w.WriteUint8(0) // al8pidmlrc
	w.WriteUB4(0)
	w.WriteUint8(0)
	if w.Caps().TTCFieldVersion >= tns.FieldVersion12_2 {
		w.WriteUint8(0) // al8sqlsig
		w.WriteUB4(0)
		w.WriteUint8(0) // SQL ID
		w.WriteUB4(0)
		w.WriteUint8(0)
		if w.Caps().TTCFieldVersion >= tns.FieldVersion12_2Ext1 {
			w.WriteUint8(0) // chunk ids
			w.WriteUB4(0)
		}
	}
	if s.cursorID == 0 || s.kind == KindDDL {
		w.WriteBytesWithLength(s.sqlBytes)
		w.WriteUB4(1) // al8i4[0] parse
	} else {
		w.WriteUB4(0)
	}
	if s.kind == KindQuery {
		if s.cursorID == 0 {
			w.WriteUB4(0)
		} else {
			w.WriteUB4(numIters)
		}
	} else {
		w.WriteUB4(uint32(m.numExecs))
	}
	w.WriteUB4(0) // al8i4[2]
	w.WriteUB4(0) // al8i4[3]
	w.WriteUB4(0) // al8i4[4]
	w.WriteUB4(0) // al8i4[5] SCN
	w.WriteUB4(0) // al8i4[6] SCN
	if s.kind == KindQuery {
		w.WriteUB4(1)
	} else {
		w.WriteUB4(0)
	}
	w.WriteUB4(0)         // al8i4[8]
	w.WriteUB4(execFlags) // al8i4[9]
	w.WriteUB4(0)         // fetch orientation
	w.WriteUB4(0)         // fetch pos
	w.WriteUB4(0)         // al8i4[12]
	if s.requiresDefine {
		for i := range s.columns {
			c := &s.columns[i]
			bufSize := c.BufferSize
			switch c.FetchType {
			case TypeLong, TypeLongRaw:
				bufSize = maxLongLength
			}
			writeColumnMetadata(w, c.FetchType, c.FetchCSForm, bufSize)
		}
	} else if numParams > 0 {
		m.writeBindParams(w)
	}
}

func (m *executeMessage) writeBindParams(w *tns.WriteBuffer) {
	for i := range m.binds {
		b := &m.binds[i]
		writeColumnMetadata(w, b.OraTypeNum, b.CSForm, b.BufferSize)
	}
	maxString := w.Caps().MaxStringSize
	isLong := func(b *BindColumn) bool {
		return m.stmt.kind != KindPLSQL && b.BufferSize > maxString
	}
	for row := 0; row < m.numExecs; row++ {
		w.WriteUint8(tns.MsgTypeRowData)
		foundLong := false
		for i := range m.binds {
			b := &m.binds[i]
			if isLong(b) {
				foundLong = true
				continue
			}
			if !m.writeBindValue(w, b, row) {
				return
			}
		}
		if foundLong {
			for i := range m.binds {
				b := &m.binds[i]
				if !isLong(b) {
					continue
				}
				if !m.writeBindValue(w, b, row) {
					return
				}
			}
		}
	}
}

func (m *executeMessage) writeBindValue(w *tns.WriteBuffer, b *BindColumn, row int) bool {
	v, err := b.Value(row)
	if err != nil {
		m.conn.deferredErr = err
		return false
	}
	if v == nil {
		if b.OraTypeNum == TypeBoolean {
			w.WriteUint8(tns.EscapeChar)
			w.WriteUint8(1)
		} else {
			w.WriteUint8(0)
		}
		return true
	}
	w.WriteBytesWithLength(v)
	return true
}

func (m *executeMessage) processMessage(r *tns.ReadBuffer, msgType uint8) error {
	return m.messageWithData.processMessage(r, msgType)
}

// postProcess is invoked by Conn.processMessage via the message
// interface hook after the response is complete.
func (m *executeMessage) finish() {
	s := m.stmt
	if !m.parseOnly {
		s.executed = true
	}
	if s.requiresDefine && !m.parseOnly {
		if m.resend {
			s.requiresDefine = false
		} else {
			m.resend = true
		}
	}
}

// fetchMessage requests more rows for an open cursor (function 5).
type fetchMessage struct {
	messageWithData
}

func (m *fetchMessage) write(w *tns.WriteBuffer) {
	m.writeFunctionCode(w)
	w.WriteUB4(uint32(m.stmt.cursorID))
	w.WriteUB4(uint32(m.arraySize))
}

func (m *fetchMessage) processMessage(r *tns.ReadBuffer, msgType uint8) error {
	return m.messageWithData.processMessage(r, msgType)
}
