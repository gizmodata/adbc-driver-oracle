package oracle

import (
	"context"
	"runtime/debug"
	"strings"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/gizmodata/adbc-driver-oracle/internal/ttc"
)

// stringRow is one row of a metadata query rendered as strings ("" for
// NULL, with a parallel null mask).
type stringRow struct {
	vals  []string
	nulls []bool
}

func (r stringRow) get(i int) string  { return r.vals[i] }
func (r stringRow) isNull(i int) bool { return r.nulls[i] }

// stringSink collects rows of a metadata query as strings.
type stringSink struct {
	stmt *ttc.Statement
	conv *arrowSink
	rows []stringRow
	cur  stringRow
	last stringRow
}

func (s *stringSink) AppendValue(col int, data []byte) error {
	c := s.stmt.Columns()[col]
	var v string
	switch c.FetchType {
	case ttc.TypeNumber, ttc.TypeBinaryInteger:
		d, err := decodeNumberText(data)
		if err != nil {
			return err
		}
		v = d
	default:
		v = string(data)
	}
	s.cur.vals = append(s.cur.vals, v)
	s.cur.nulls = append(s.cur.nulls, false)
	return nil
}
func (s *stringSink) AppendNull(col int) error {
	s.cur.vals = append(s.cur.vals, "")
	s.cur.nulls = append(s.cur.nulls, true)
	return nil
}
func (s *stringSink) AppendDuplicate(col int) error {
	s.cur.vals = append(s.cur.vals, s.last.vals[col])
	s.cur.nulls = append(s.cur.nulls, s.last.nulls[col])
	return nil
}
func (s *stringSink) FinishRow() error {
	s.rows = append(s.rows, s.cur)
	s.last = s.cur
	s.cur = stringRow{}
	return nil
}

// queryAll runs sql and materializes every row as strings (for small
// metadata queries only).
func (c *connectionImpl) queryAll(ctx context.Context, sql string, binds ...ttc.BindColumn) ([]stringRow, error) {
	st := c.conn.Prepare(sql)
	defer st.Close()
	sink := &stringSink{stmt: st}
	if err := st.Execute(ctx, ttc.ExecuteOptions{Binds: binds, PrefetchRows: 1000, ArraySize: 1000, Sink: sink, AutoCommit: false}); err != nil {
		return nil, fromTTCError(err)
	}
	for st.MoreRows() {
		if err := st.Fetch(ctx, sink, 1000); err != nil {
			return nil, fromTTCError(err)
		}
	}
	return sink.rows, nil
}

func strBind(v string) ttc.BindColumn {
	return ttc.BindColumn{OraTypeNum: ttc.TypeVarchar, CSForm: ttc.CSFormImplicit, BufferSize: uint32(max(len(v), 1)), Value: func(int) ([]byte, error) {
		if v == "" {
			return nil, nil
		}
		return []byte(v), nil
	}}
}

func (c *connectionImpl) currentSchema(ctx context.Context) (string, error) {
	rows, err := c.queryAll(ctx, "SELECT SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA') FROM DUAL")
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].get(0), nil
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ---- GetInfo ----

var supportedInfoCodes = []adbc.InfoCode{
	adbc.InfoVendorName,
	adbc.InfoVendorVersion,
	adbc.InfoVendorSql,
	adbc.InfoVendorSubstrait,
	adbc.InfoDriverName,
	adbc.InfoDriverVersion,
	adbc.InfoDriverArrowVersion,
	adbc.InfoDriverADBCVersion,
}

func driverVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
	}
	return Version
}

// arrowVersion is the arrow-go module version this binary was built with (e.g. "v18.8.0"),
// read from the build info the way arrow-adbc's driverbase does.
func arrowVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if !strings.HasPrefix(dep.Path, "github.com/apache/arrow-go/") {
				continue
			}
			if dep.Replace != nil && dep.Replace.Version != "" {
				return dep.Replace.Version
			}
			if dep.Version != "" {
				return dep.Version
			}
		}
	}
	return "v18"
}

func (c *connectionImpl) getInfoImpl(ctx context.Context, infoCodes []adbc.InfoCode) (array.RecordReader, error) {
	if len(infoCodes) == 0 {
		infoCodes = supportedInfoCodes
	}
	bldr := array.NewRecordBuilder(c.alloc, adbc.GetInfoSchema)
	defer bldr.Release()
	nameBldr := bldr.Field(0).(*array.Uint32Builder)
	valueBldr := bldr.Field(1).(*array.DenseUnionBuilder)
	strBldr := valueBldr.Child(int(adbc.InfoValueStringType)).(*array.StringBuilder)
	int64Bldr := valueBldr.Child(int(adbc.InfoValueInt64Type)).(*array.Int64Builder)
	boolBldr := valueBldr.Child(int(adbc.InfoValueBooleanType)).(*array.BooleanBuilder)

	for _, code := range infoCodes {
		nameBldr.Append(uint32(code))
		switch code {
		case adbc.InfoVendorName:
			valueBldr.Append(int8(adbc.InfoValueStringType))
			strBldr.Append(VendorName)
		case adbc.InfoVendorVersion:
			valueBldr.Append(int8(adbc.InfoValueStringType))
			strBldr.Append(c.conn.ServerVersionString())
		case adbc.InfoVendorSql:
			valueBldr.Append(int8(adbc.InfoValueBooleanType))
			boolBldr.Append(true)
		case adbc.InfoVendorSubstrait:
			valueBldr.Append(int8(adbc.InfoValueBooleanType))
			boolBldr.Append(false)
		case adbc.InfoDriverName:
			valueBldr.Append(int8(adbc.InfoValueStringType))
			strBldr.Append(DriverName)
		case adbc.InfoDriverVersion:
			valueBldr.Append(int8(adbc.InfoValueStringType))
			strBldr.Append(driverVersion())
		case adbc.InfoDriverArrowVersion:
			valueBldr.Append(int8(adbc.InfoValueStringType))
			strBldr.Append(arrowVersion())
		case adbc.InfoDriverADBCVersion:
			valueBldr.Append(int8(adbc.InfoValueInt64Type))
			int64Bldr.Append(adbc.AdbcVersion1_1_0)
		default:
			valueBldr.Append(int8(adbc.InfoValueStringType))
			strBldr.AppendNull()
		}
	}
	rec := bldr.NewRecord()
	defer rec.Release()
	return array.NewRecordReader(adbc.GetInfoSchema, []arrow.Record{rec})
}

// ---- GetTableTypes ----

var oracleTableTypes = []string{"TABLE", "VIEW", "MATERIALIZED VIEW", "SYNONYM", "GLOBAL TEMPORARY"}

func (c *connectionImpl) getTableTypesImpl(context.Context) (array.RecordReader, error) {
	bldr := array.NewRecordBuilder(c.alloc, adbc.TableTypesSchema)
	defer bldr.Release()
	tb := bldr.Field(0).(*array.StringBuilder)
	for _, t := range oracleTableTypes {
		tb.Append(t)
	}
	rec := bldr.NewRecord()
	defer rec.Release()
	return array.NewRecordReader(adbc.TableTypesSchema, []arrow.Record{rec})
}

// ---- GetTableSchema ----

func (c *connectionImpl) getTableSchemaImpl(ctx context.Context, catalog, dbSchema *string, tableName string) (*arrow.Schema, error) {
	// Names are matched exactly first, as GetObjects reports them (a table
	// created as "mixedCase" is found by that name); then, for compatibility,
	// the way Oracle reads them unquoted (customers -> CUSTOMERS).
	quote := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	qualify := func(ident func(string) string) string {
		if dbSchema != nil && *dbSchema != "" {
			return ident(*dbSchema) + "." + ident(tableName)
		}
		return ident(tableName)
	}
	candidates := []string{qualify(quote)}
	if folded := qualify(identFor); folded != candidates[0] {
		candidates = append(candidates, folded)
	}
	var err error
	for _, qualified := range candidates {
		st := c.conn.Prepare("SELECT * FROM " + qualified)
		err = st.Describe(ctx)
		if err == nil {
			schema := schemaFor(st.Columns(), c.cfg.types)
			st.Close()
			return schema, nil
		}
		st.Close()
		var ae adbc.Error
		if err = fromTTCError(err); !asADBC(err, &ae) || ae.VendorCode != 942 {
			return nil, err
		}
	}
	return nil, err
}
