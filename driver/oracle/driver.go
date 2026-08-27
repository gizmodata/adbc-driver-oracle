package oracle

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/gizmodata/adbc-driver-oracle/internal/ttc"
)

// NewDriver returns an Oracle ADBC driver.
func NewDriver(alloc memory.Allocator) adbc.Driver {
	if alloc == nil {
		alloc = memory.NewGoAllocator()
	}
	return &driverImpl{alloc: alloc}
}

type driverImpl struct {
	alloc memory.Allocator
}

func (d *driverImpl) NewDatabase(opts map[string]string) (adbc.Database, error) {
	return d.NewDatabaseWithContext(context.Background(), opts)
}

func (d *driverImpl) NewDatabaseWithContext(_ context.Context, opts map[string]string) (adbc.Database, error) {
	return &databaseImpl{alloc: d.alloc, opts: cloneMap(opts)}, nil
}

type databaseImpl struct {
	alloc memory.Allocator
	mu    sync.Mutex
	opts  map[string]string
}

func (d *databaseImpl) SetOptions(opts map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range opts {
		d.opts[k] = v
	}
	return nil
}

func (d *databaseImpl) Open(ctx context.Context) (adbc.Connection, error) {
	d.mu.Lock()
	opts := cloneMap(d.opts)
	d.mu.Unlock()
	cfg, err := parseOptions(opts)
	if err != nil {
		return nil, err
	}
	if cfg.trace {
		cfg.ttc.Trace = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "[adbc-oracle] "+format+"\n", args...)
		}
	}
	conn, err := ttc.Dial(ctx, &cfg.ttc)
	if err != nil {
		return nil, fromTTCError(err)
	}
	return &connectionImpl{
		db:         d,
		conn:       conn,
		alloc:      d.alloc,
		cfg:        cfg,
		autoCommit: true,
	}, nil
}

func (d *databaseImpl) Close() error { return nil }

// connectionImpl implements adbc.Connection over one TTC session.
//
// Transaction model: with autocommit on (the ADBC default) every
// statement is executed with the server-side COMMIT flag; with
// autocommit off nothing is committed until Commit/Rollback.
type connectionImpl struct {
	db         *databaseImpl
	conn       *ttc.Conn
	alloc      memory.Allocator
	cfg        *connConfig
	autoCommit bool
	closed     bool
	mu         sync.Mutex
}

func (c *connectionImpl) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errStatus(adbc.StatusInvalidState, "connection already closed")
	}
	c.closed = true
	return c.conn.Close()
}

func (c *connectionImpl) NewStatement() (adbc.Statement, error) {
	if c.closed {
		return nil, errStatus(adbc.StatusInvalidState, "connection is closed")
	}
	return &statementImpl{conn: c, alloc: c.alloc}, nil
}

func (c *connectionImpl) GetInfo(ctx context.Context, codes []adbc.InfoCode) (array.RecordReader, error) {
	return c.getInfoImpl(ctx, codes)
}

func (c *connectionImpl) GetObjects(ctx context.Context, depth adbc.ObjectDepth, catalog, dbSchema, tableName *string, columnName *string, tableTypes []string) (array.RecordReader, error) {
	return c.getObjectsImpl(ctx, depth, catalog, dbSchema, tableName, columnName, tableTypes)
}

func (c *connectionImpl) GetTableSchema(ctx context.Context, catalog, dbSchema *string, tableName string) (*arrow.Schema, error) {
	return c.getTableSchemaImpl(ctx, catalog, dbSchema, tableName)
}

func (c *connectionImpl) GetTableTypes(ctx context.Context) (array.RecordReader, error) {
	return c.getTableTypesImpl(ctx)
}

func (c *connectionImpl) Commit(ctx context.Context) error {
	if c.autoCommit {
		return errStatus(adbc.StatusInvalidState, "Commit called while autocommit is enabled")
	}
	return fromTTCError(c.conn.Commit(ctx))
}

func (c *connectionImpl) Rollback(ctx context.Context) error {
	if c.autoCommit {
		return errStatus(adbc.StatusInvalidState, "Rollback called while autocommit is enabled")
	}
	return fromTTCError(c.conn.Rollback(ctx))
}

func (c *connectionImpl) SetOption(key, value string) error {
	switch key {
	case adbc.OptionKeyAutoCommit:
		switch value {
		case adbc.OptionValueEnabled:
			if !c.autoCommit && c.conn.TxnInProgress() {
				if err := c.conn.Commit(context.Background()); err != nil {
					return fromTTCError(err)
				}
			}
			c.autoCommit = true
		case adbc.OptionValueDisabled:
			c.autoCommit = false
		default:
			return errStatus(adbc.StatusInvalidArgument, "unknown value %q for %s", value, key)
		}
		return nil
	case adbc.OptionKeyCurrentDbSchema:
		c.conn.SetCurrentSchema(value)
		return fromTTCError(c.conn.Ping(context.Background()))
	case adbc.OptionKeyIsolationLevel:
		var stmt string
		switch value {
		case string(adbc.LevelDefault), string(adbc.LevelReadCommitted):
			stmt = "ALTER SESSION SET ISOLATION_LEVEL = READ COMMITTED"
		case string(adbc.LevelSerializable), string(adbc.LevelSnapshot), string(adbc.LevelRepeatableRead), string(adbc.LevelLinearizable):
			stmt = "ALTER SESSION SET ISOLATION_LEVEL = SERIALIZABLE"
		default:
			return errStatus(adbc.StatusNotImplemented, "isolation level %q is not supported by Oracle", value)
		}
		return c.execNoResult(context.Background(), stmt)
	case adbc.OptionKeyReadOnly:
		switch value {
		case adbc.OptionValueEnabled:
			return c.execNoResult(context.Background(), "SET TRANSACTION READ ONLY")
		case adbc.OptionValueDisabled:
			return nil
		}
		return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
	case OptionBatchSize:
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
		}
		c.cfg.batchSize = n
		return nil
	case OptionPrefetchRows:
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
		}
		c.cfg.prefetchRows = n
		return nil
	case OptionNumberMode:
		nm, err := parseNumberMode(value)
		if err != nil {
			return err
		}
		c.cfg.types.numberMode = nm
		return nil
	case OptionIntervalMode:
		im, err := parseIntervalMode(value)
		if err != nil {
			return err
		}
		c.cfg.types.intervalMode = im
		return nil
	case OptionDateMode:
		dm, err := parseDateMode(value)
		if err != nil {
			return err
		}
		c.cfg.types.dateMode = dm
		return nil
	case OptionBatchBytes:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
		}
		c.cfg.batchBytes = n
		return nil
	case OptionUseExtensionTypes:
		c.cfg.types.useExtensionTypes = isTrue(value)
		return nil
	case "adbc.oracle.module", "adbc.oracle.action", "adbc.oracle.client_info", "adbc.oracle.client_identifier":
		var which uint8
		switch key {
		case "adbc.oracle.module":
			which = 1
		case "adbc.oracle.action":
			which = 2
		case "adbc.oracle.client_info":
			which = 4
		default:
			which = 8
		}
		c.conn.SetClientInfo(value, value, value, value, which)
		return nil
	}
	return errStatus(adbc.StatusNotImplemented, "unknown connection option %q", key)
}

func (c *connectionImpl) GetOption(key string) (string, error) {
	switch key {
	case adbc.OptionKeyAutoCommit:
		if c.autoCommit {
			return adbc.OptionValueEnabled, nil
		}
		return adbc.OptionValueDisabled, nil
	case adbc.OptionKeyCurrentCatalog:
		return c.conn.DBName(), nil
	case adbc.OptionKeyCurrentDbSchema:
		return c.currentSchema(context.Background())
	case OptionBatchSize:
		return strconv.Itoa(c.cfg.batchSize), nil
	case OptionPrefetchRows:
		return strconv.Itoa(c.cfg.prefetchRows), nil
	case OptionNumberMode:
		return c.cfg.types.numberMode, nil
	case OptionIntervalMode:
		return c.cfg.types.intervalMode, nil
	case OptionDateMode:
		return c.cfg.types.dateMode, nil
	case OptionBatchBytes:
		return strconv.FormatInt(c.cfg.batchBytes, 10), nil
	case OptionUseExtensionTypes:
		if c.cfg.types.useExtensionTypes {
			return adbc.OptionValueEnabled, nil
		}
		return adbc.OptionValueDisabled, nil
	}
	return "", errStatus(adbc.StatusNotFound, "unknown connection option %q", key)
}

func (c *connectionImpl) ReadPartition(context.Context, []byte) (array.RecordReader, error) {
	return nil, errStatus(adbc.StatusNotImplemented, "ReadPartition")
}

// execNoResult executes a statement without a result set.
func (c *connectionImpl) execNoResult(ctx context.Context, sql string) error {
	st := c.conn.Prepare(sql)
	defer st.Close()
	return fromTTCError(st.Execute(ctx, ttc.ExecuteOptions{AutoCommit: c.autoCommit}))
}

// statementImpl implements adbc.Statement.
type statementImpl struct {
	conn                *connectionImpl
	alloc               memory.Allocator
	sql                 string
	targetTable         string
	targetSchema        string
	ingestMode          string
	ingestTemporary     bool
	ingestBatchRows     int
	ingestVarcharLength int
	ingestRawLength     int
	ingestStringType    string
	ingestBinaryType    string
	ingestStructType    string
	ingestTablespace    string
	closed              bool
	bound               arrow.Record
	boundStream         array.RecordReader
	prepared            *ttc.Statement
	bindSchema          *arrow.Schema // schema of the last bound parameters
}

func (s *statementImpl) Close() error {
	if s.closed {
		return errStatus(adbc.StatusInvalidState, "statement already closed")
	}
	s.closed = true
	s.clearBound()
	if s.prepared != nil {
		s.prepared.Close()
		s.prepared = nil
	}
	return nil
}

func (s *statementImpl) clearBound() {
	if s.bound != nil {
		s.bound.Release()
		s.bound = nil
	}
	if s.boundStream != nil {
		s.boundStream.Release()
		s.boundStream = nil
	}
}

func (s *statementImpl) SetSqlQuery(sql string) error {
	s.sql = sql
	if s.prepared != nil {
		s.prepared.Close()
		s.prepared = nil
	}
	return nil
}

func (s *statementImpl) SetOption(key, value string) error {
	switch key {
	case adbc.OptionKeyIngestTargetTable:
		s.targetTable = value
	case adbc.OptionValueIngestTargetDBSchema:
		s.targetSchema = value
	case adbc.OptionKeyIngestMode:
		switch value {
		case adbc.OptionValueIngestModeCreate, adbc.OptionValueIngestModeAppend,
			adbc.OptionValueIngestModeReplace, adbc.OptionValueIngestModeCreateAppend:
			s.ingestMode = value
		default:
			return errStatus(adbc.StatusInvalidArgument, "unknown ingest mode %q", value)
		}
	case adbc.OptionValueIngestTemporary:
		switch value {
		case adbc.OptionValueEnabled:
			s.ingestTemporary = true
		case adbc.OptionValueDisabled:
			s.ingestTemporary = false
		default:
			return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
		}
	case OptionIngestBatchRows:
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
		}
		s.ingestBatchRows = n
	case OptionIngestVarcharLength:
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
		}
		s.ingestVarcharLength = n
	case OptionIngestRawLength:
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
		}
		s.ingestRawLength = n
	case OptionIngestStringType:
		switch strings.ToUpper(value) {
		case "VARCHAR2", "CLOB", "NVARCHAR2", "NCLOB":
			s.ingestStringType = strings.ToUpper(value)
		default:
			return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
		}
	case OptionIngestBinaryType:
		switch strings.ToUpper(value) {
		case "RAW", "BLOB":
			s.ingestBinaryType = strings.ToUpper(value)
		default:
			return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
		}
	case OptionIngestStructType:
		switch strings.ToUpper(value) {
		case "JSON", "CLOB", "VARCHAR2", "BLOB":
			s.ingestStructType = strings.ToUpper(value)
		default:
			return errStatus(adbc.StatusInvalidArgument, "invalid value %q for %s", value, key)
		}
	case OptionIngestTablespace:
		s.ingestTablespace = value
	default:
		return errStatus(adbc.StatusNotImplemented, "unknown statement option %q", key)
	}
	return nil
}

func (s *statementImpl) SetSubstraitPlan([]byte) error {
	return errStatus(adbc.StatusNotImplemented, "Substrait")
}

// Prepare parses the SQL and, for queries, describes the result set so
// GetParameterSchema / ExecuteSchema work without executing.
func (s *statementImpl) Prepare(ctx context.Context) error {
	if s.sql == "" {
		return errStatus(adbc.StatusInvalidState, "Prepare: no SQL set")
	}
	s.prepared = s.conn.conn.Prepare(rewriteQmark(s.sql))
	return nil
}

func (s *statementImpl) statement() *ttc.Statement {
	if s.prepared == nil {
		s.prepared = s.conn.conn.Prepare(rewriteQmark(s.sql))
	}
	return s.prepared
}

func (s *statementImpl) ExecuteQuery(ctx context.Context) (array.RecordReader, int64, error) {
	if s.closed {
		return nil, -1, errStatus(adbc.StatusInvalidState, "statement is closed")
	}
	if s.targetTable != "" && (s.bound != nil || s.boundStream != nil) {
		n, err := s.executeIngest(ctx)
		if err != nil {
			return nil, -1, err
		}
		rr, err := array.NewRecordReader(arrow.NewSchema([]arrow.Field{}, nil), nil)
		if err != nil {
			return nil, -1, err
		}
		return rr, n, nil
	}
	if s.sql == "" {
		return nil, -1, errStatus(adbc.StatusInvalidState, "ExecuteQuery: no SQL set")
	}
	st := s.statement()
	binds, numRows, err := s.bindColumns(st)
	if err != nil {
		return nil, -1, err
	}
	defer s.clearBound()
	if !st.IsQuery() {
		if err := st.Execute(ctx, ttc.ExecuteOptions{Binds: binds, NumRows: numRows, AutoCommit: s.conn.autoCommit}); err != nil {
			return nil, -1, fromTTCError(err)
		}
		// A PL/SQL block may hand back an implicit result set
		// (DBMS_SQL.RETURN_RESULT) — stream it like a query.
		if child := st.ImplicitResult(); child != nil {
			reader := newStreamingRecordReader(ctx, s.conn, child)
			if err := reader.start(); err != nil {
				reader.abandon()
				return nil, -1, err
			}
			return reader, -1, nil
		}
		// OUT / IN OUT binds come back as a one-row result set.
		if hasOutBinds(st) {
			rec, err := recordFromOutBinds(s.alloc, st, binds, s.bindSchema, s.conn.cfg.types)
			if err != nil {
				return nil, -1, err
			}
			defer rec.Release()
			rr, err := array.NewRecordReader(rec.Schema(), []arrow.Record{rec})
			if err != nil {
				return nil, -1, err
			}
			return rr, int64(st.RowCount()), nil
		}
		rr, err := array.NewRecordReader(arrow.NewSchema([]arrow.Field{}, nil), nil)
		if err != nil {
			return nil, -1, err
		}
		return rr, int64(st.RowCount()), nil
	}
	reader := newStreamingRecordReader(ctx, s.conn, st)
	if err := st.Execute(ctx, ttc.ExecuteOptions{
		Binds:        binds,
		NumRows:      numRows,
		PrefetchRows: s.conn.cfg.prefetchRows,
		ArraySize:    s.conn.cfg.prefetchRows,
		AutoCommit:   s.conn.autoCommit,
		Sink:         reader.sink,
	}); err != nil {
		reader.abandon()
		return nil, -1, fromTTCError(err)
	}
	if err := reader.start(); err != nil {
		reader.abandon()
		return nil, -1, err
	}
	// The statement is now owned by the reader until it is released; the
	// next ExecuteQuery on this ADBC statement re-prepares.
	s.prepared = nil
	return reader, -1, nil
}

func (s *statementImpl) ExecuteUpdate(ctx context.Context) (int64, error) {
	if s.closed {
		return -1, errStatus(adbc.StatusInvalidState, "statement is closed")
	}
	if s.targetTable != "" && (s.bound != nil || s.boundStream != nil) {
		return s.executeIngest(ctx)
	}
	if s.sql == "" {
		return -1, errStatus(adbc.StatusInvalidState, "ExecuteUpdate: no SQL set")
	}
	st := s.statement()
	binds, numRows, err := s.bindColumns(st)
	if err != nil {
		return -1, err
	}
	defer s.clearBound()
	if st.IsQuery() {
		// Run it and discard rows.
		if err := st.Execute(ctx, ttc.ExecuteOptions{Binds: binds, NumRows: numRows, AutoCommit: s.conn.autoCommit, PrefetchRows: 1}); err != nil {
			return -1, fromTTCError(err)
		}
		st.Close()
		s.prepared = nil
		return -1, nil
	}
	if err := st.Execute(ctx, ttc.ExecuteOptions{Binds: binds, NumRows: numRows, AutoCommit: s.conn.autoCommit}); err != nil {
		return -1, fromTTCError(err)
	}
	return int64(st.RowCount()), nil
}

func (s *statementImpl) ExecuteSchema(ctx context.Context) (*arrow.Schema, error) {
	if s.sql == "" {
		return nil, errStatus(adbc.StatusInvalidState, "ExecuteSchema: no SQL set")
	}
	st := s.statement()
	if err := st.Describe(ctx); err != nil {
		return nil, fromTTCError(err)
	}
	return schemaFor(st.Columns(), s.conn.cfg.types), nil
}

// Cancel interrupts the statement's in-progress server call (ADBC 1.1).
func (s *statementImpl) Cancel() error {
	s.conn.conn.Cancel()
	return nil
}

// Cancel interrupts whatever call is in progress on the connection.
func (c *connectionImpl) Cancel() error {
	c.conn.Cancel()
	return nil
}

func hasOutBinds(st *ttc.Statement) bool {
	for _, d := range st.OutBindDirs() {
		if d != ttc.BindDirInput {
			return true
		}
	}
	return false
}

func (s *statementImpl) GetParameterSchema() (*arrow.Schema, error) {
	st := s.statement()
	names := st.BindNames()
	fields := make([]arrow.Field, len(names))
	for i, n := range names {
		fields[i] = arrow.Field{Name: n, Type: arrow.Null, Nullable: true}
	}
	return arrow.NewSchema(fields, nil), nil
}

func (s *statementImpl) Bind(_ context.Context, rec arrow.Record) error {
	rec.Retain()
	s.clearBound()
	s.bound = rec
	return nil
}

func (s *statementImpl) BindStream(_ context.Context, rr array.RecordReader) error {
	rr.Retain()
	s.clearBound()
	s.boundStream = rr
	return nil
}

func (s *statementImpl) ExecutePartitions(context.Context) (*arrow.Schema, adbc.Partitions, int64, error) {
	return nil, adbc.Partitions{}, -1, errStatus(adbc.StatusNotImplemented, "ExecutePartitions")
}

// bindColumns converts the bound record (or the first batch of a bound
// stream) into TTC bind columns. For non-query statements every row of
// the record is executed (array DML).
func (s *statementImpl) bindColumns(st *ttc.Statement) ([]ttc.BindColumn, int, error) {
	var rec arrow.Record
	if s.bound != nil {
		rec = s.bound
	} else if s.boundStream != nil {
		if !s.boundStream.Next() {
			if err := s.boundStream.Err(); err != nil {
				return nil, 0, errStatus(adbc.StatusIO, "bound stream: %v", err)
			}
			return nil, 0, errStatus(adbc.StatusInvalidArgument, "bound stream produced no batches")
		}
		rec = s.boundStream.Record()
	} else {
		if len(st.BindNames()) > 0 {
			return nil, 0, errStatus(adbc.StatusInvalidArgument, "statement has %d bind variables but no parameters were bound", len(st.BindNames()))
		}
		return nil, 1, nil
	}
	if int(rec.NumCols()) != len(st.BindNames()) {
		return nil, 0, errStatus(adbc.StatusInvalidArgument, "statement has %d bind variables but %d parameter columns were bound", len(st.BindNames()), rec.NumCols())
	}
	binds, err := bindsFromRecord(rec, s.conn.conn)
	if err != nil {
		return nil, 0, err
	}
	s.bindSchema = rec.Schema()
	n := int(rec.NumRows())
	if n == 0 {
		n = 1
	}
	return binds, n, nil
}

// rewriteQmark converts JDBC/DBAPI-style "?" placeholders to Oracle
// positional binds (:1, :2, ...), leaving quoted strings and comments
// untouched. Statements that already use :name binds are returned as is.
func rewriteQmark(sql string) string {
	if !strings.Contains(sql, "?") {
		return sql
	}
	var sb strings.Builder
	sb.Grow(len(sql) + 8)
	n := 0
	inSingle, inDouble, inLine, inBlock := false, false, false, false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			}
		case inBlock:
			if c == '*' && i+1 < len(sql) && sql[i+1] == '/' {
				inBlock = false
				sb.WriteByte(c)
				i++
				c = '/'
			}
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if c == '"' {
				inDouble = false
			}
		default:
			switch c {
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case '-':
				if i+1 < len(sql) && sql[i+1] == '-' {
					inLine = true
				}
			case '/':
				if i+1 < len(sql) && sql[i+1] == '*' {
					inBlock = true
				}
			case '?':
				n++
				sb.WriteString(":")
				sb.WriteString(strconv.Itoa(n))
				continue
			}
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
