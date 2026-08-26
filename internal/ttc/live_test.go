package ttc

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gizmodata/adbc-driver-oracle/internal/oratype"
)

// rowCollector is a RowSink that renders every value as text.
type rowCollector struct {
	cols []Column
	rows [][]string
	cur  []string
	last []string
}

func (s *rowCollector) AppendValue(col int, data []byte) error {
	var v string
	c := s.cols[col]
	switch c.FetchType {
	case TypeNumber, TypeBinaryInteger:
		d, err := oratype.DecodeNumber(data, nil)
		if err != nil {
			return err
		}
		v = string(d.Text)
	case TypeDate, TypeTimestamp, TypeTimestampTZ, TypeTimestampLTZ:
		d, err := oratype.DecodeDate(data)
		if err != nil {
			return err
		}
		v = time.Unix(0, d.UnixNanos()).UTC().Format(time.RFC3339Nano)
	case TypeBinaryDouble:
		f, _ := oratype.DecodeBinaryDouble(data)
		v = strconv.FormatFloat(f, 'g', -1, 64)
	case TypeRaw, TypeLongRaw:
		v = "0x" + hexOf(data)
	default:
		v = string(data)
	}
	s.cur = append(s.cur, v)
	return nil
}
func (s *rowCollector) AppendNull(col int) error      { s.cur = append(s.cur, "<NULL>"); return nil }
func (s *rowCollector) AppendDuplicate(col int) error { s.cur = append(s.cur, s.last[col]); return nil }
func (s *rowCollector) FinishRow() error {
	s.rows = append(s.rows, s.cur)
	s.last = s.cur
	s.cur = nil
	return nil
}

func formatFloat(f float64) string {
	return strings.TrimSpace(strings.Replace(strings.Replace(fmtFloat(f), "e+00", "", 1), "+", "", 1))
}
func fmtFloat(f float64) string { return strings.TrimSpace(fmt.Sprintf("%f", f)) }
func hexOf(b []byte) string {
	const hx = "0123456789ABCDEF"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hx[c>>4], hx[c&15])
	}
	return string(out)
}

func liveConfig(t *testing.T) *Config {
	host := os.Getenv("ORACLE_TEST_HOST")
	if host == "" {
		host = "localhost"
	}
	conn, err := net.DialTimeout("tcp", host+":1521", 2*time.Second)
	if err != nil {
		t.Skipf("no Oracle listener at %s:1521: %v", host, err)
	}
	conn.Close()
	cfg := &Config{
		Addresses:   []Address{{Host: host, Port: 1521}},
		ServiceName: "FREEPDB1",
		Username:    "scott",
		Password:    "tiger",
		FullVersion: 1 << 20,
	}
	if os.Getenv("ORACLE_TRACE") != "" {
		cfg.Trace = func(format string, args ...any) { t.Logf(format, args...) }
	}
	return cfg
}

func TestLiveConnectAndQuery(t *testing.T) {
	ctx := context.Background()
	c, err := Dial(ctx, liveConfig(t))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	t.Logf("server version %s banner %q session %v", c.ServerVersionString(), c.ServerBanner(), c.sessionID)

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	stmt := c.Prepare("SELECT 1 AS n, 'hello' AS s, SYSDATE AS d, NULL AS x, 2.5 AS f, CAST(3.25 AS BINARY_DOUBLE) AS bd FROM dual")
	sink := &rowCollector{}
	err = stmt.Execute(ctx, ExecuteOptions{Sink: &deferredSink{inner: sink, stmt: stmt}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, col := range stmt.Columns() {
		t.Logf("col %-6s type=%-3d (%s) prec=%d scale=%d size=%d buf=%d null=%v csform=%d", col.Name, col.OraTypeNum, col.TypeName(), col.Precision, col.Scale, col.MaxSize, col.BufferSize, col.NullsAllowed, col.CSForm)
	}
	// rows delivered during execute were appended before cols known; re-run
	sink2 := &rowCollector{}
	stmt2 := c.Prepare("SELECT 1 AS n, 'hello' AS s, SYSDATE AS d, NULL AS x, 2.5 AS f, CAST(3.25 AS BINARY_DOUBLE) AS bd FROM dual")
	if err := stmt2.Execute(ctx, ExecuteOptions{Sink: &deferredSink{inner: sink2, stmt: stmt2}}); err != nil {
		t.Fatalf("execute2: %v", err)
	}
	t.Logf("rows: %v more=%v", sink2.rows, stmt2.MoreRows())
	if len(sink2.rows) != 1 || sink2.rows[0][0] != "1" || sink2.rows[0][1] != "hello" || sink2.rows[0][3] != "<NULL>" || sink2.rows[0][4] != "2.5" {
		t.Fatalf("unexpected rows %v", sink2.rows)
	}
	stmt.Close()
	stmt2.Close()

	// multi-batch fetch
	stmt3 := c.Prepare("SELECT level AS n, 'row' || level AS s FROM dual CONNECT BY level <= 2500")
	sink3 := &rowCollector{}
	if err := stmt3.Execute(ctx, ExecuteOptions{PrefetchRows: 100, ArraySize: 1000}); err != nil {
		t.Fatalf("execute3: %v", err)
	}
	sink3.cols = stmt3.Columns()
	// first batch was delivered to nil sink (discarded) — re-execute with sink
	stmt3.Close()
	stmt3 = c.Prepare("SELECT level AS n, 'row' || level AS s FROM dual CONNECT BY level <= 2500")
	sink3 = &rowCollector{}
	sink3.cols = nil
	holder := &deferredSink{inner: sink3, stmt: stmt3}
	if err := stmt3.Execute(ctx, ExecuteOptions{PrefetchRows: 100, ArraySize: 1000, Sink: holder}); err != nil {
		t.Fatalf("execute3b: %v", err)
	}
	for stmt3.MoreRows() {
		if err := stmt3.Fetch(ctx, holder, 1000); err != nil {
			t.Fatalf("fetch: %v", err)
		}
	}
	if len(sink3.rows) != 2500 || sink3.rows[2499][0] != "2500" || sink3.rows[2499][1] != "row2500" {
		t.Fatalf("got %d rows, last %v", len(sink3.rows), sink3.rows[len(sink3.rows)-1])
	}
	t.Logf("fetched %d rows", len(sink3.rows))
	stmt3.Close()

	// error handling
	bad := c.Prepare("SELECT * FROM no_such_table_xyz")
	err = bad.Execute(ctx, ExecuteOptions{})
	if oe, ok := AsError(err); !ok || oe.Code != 942 {
		t.Fatalf("expected ORA-00942, got %v", err)
	}
	t.Logf("error ok: %v", err)
	bad.Close()

	// connection still usable
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping after error: %v", err)
	}
}

// deferredSink resolves column metadata lazily (available after describe).
type deferredSink struct {
	inner *rowCollector
	stmt  *Statement
}

func (d *deferredSink) ensure() {
	if d.inner.cols == nil {
		d.inner.cols = d.stmt.Columns()
	}
}
func (d *deferredSink) AppendValue(col int, data []byte) error {
	d.ensure()
	return d.inner.AppendValue(col, data)
}
func (d *deferredSink) AppendNull(col int) error { d.ensure(); return d.inner.AppendNull(col) }
func (d *deferredSink) AppendDuplicate(col int) error {
	d.ensure()
	return d.inner.AppendDuplicate(col)
}
func (d *deferredSink) FinishRow() error { return d.inner.FinishRow() }

func TestLiveDMLAndBinds(t *testing.T) {
	ctx := context.Background()
	c, err := Dial(ctx, liveConfig(t))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	exec := func(sql string) {
		t.Helper()
		st := c.Prepare(sql)
		if err := st.Execute(ctx, ExecuteOptions{}); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		st.Close()
	}
	c.Prepare("DROP TABLE ttc_live_test PURGE").Execute(ctx, ExecuteOptions{})
	exec("CREATE TABLE ttc_live_test (id NUMBER(10), name VARCHAR2(100), amt NUMBER(12,2), ts TIMESTAMP(6), raw_col RAW(16), big CLOB)")

	names := []string{"alpha", "beta", "gamma", "delta"}
	ins := c.Prepare("INSERT INTO ttc_live_test (id, name, amt, ts, raw_col, big) VALUES (:1, :2, :3, :4, :5, :6)")
	bigText := strings.Repeat("x", 40000)
	binds := []BindColumn{
		{OraTypeNum: TypeNumber, BufferSize: 22, Value: func(i int) ([]byte, error) { return oratype.EncodeInt64(nil, int64(i+1)) }},
		{OraTypeNum: TypeVarchar, CSForm: CSFormImplicit, BufferSize: 100, Value: func(i int) ([]byte, error) { return []byte(names[i]), nil }},
		{OraTypeNum: TypeNumber, BufferSize: 22, Value: func(i int) ([]byte, error) {
			if i == 2 {
				return nil, nil
			}
			return oratype.EncodeNumber(nil, []byte("12.34"))
		}},
		{OraTypeNum: TypeTimestamp, BufferSize: 11, Value: func(i int) ([]byte, error) {
			return oratype.EncodeTimestamp(nil, time.Date(2024, 3, 4, 5, 6, 7, 123456000, time.UTC)), nil
		}},
		{OraTypeNum: TypeRaw, BufferSize: 16, Value: func(i int) ([]byte, error) { return []byte{1, 2, 3, byte(i)}, nil }},
		{OraTypeNum: TypeLong, CSForm: CSFormImplicit, BufferSize: 0x7fffffff, Value: func(i int) ([]byte, error) { return []byte(bigText), nil }},
	}
	if err := ins.Execute(ctx, ExecuteOptions{Binds: binds, NumRows: len(names), AutoCommit: true}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Logf("inserted rowcount=%d", ins.RowCount())
	if ins.RowCount() != 4 {
		t.Fatalf("rowcount %d", ins.RowCount())
	}
	ins.Close()

	q := c.Prepare("SELECT id, name, amt, ts, raw_col, LENGTH(big) AS biglen, big FROM ttc_live_test ORDER BY id")
	sink := &rowCollector{}
	ds := &deferredSink{inner: sink, stmt: q}
	if err := q.Execute(ctx, ExecuteOptions{Sink: ds}); err != nil {
		t.Fatalf("select: %v", err)
	}
	for q.MoreRows() {
		if err := q.Fetch(ctx, ds, 100); err != nil {
			t.Fatalf("fetch: %v", err)
		}
	}
	for _, col := range q.Columns() {
		t.Logf("col %-8s type=%-3d fetch=%-3d prec=%d scale=%d size=%d buf=%d", col.Name, col.OraTypeNum, col.FetchType, col.Precision, col.Scale, col.MaxSize, col.BufferSize)
	}
	for _, r := range sink.rows {
		r6 := r[6]
		if len(r6) > 20 {
			r6 = r6[:20] + "..."
		}
		t.Logf("row: %v %s", r[:6], r6)
	}
	if len(sink.rows) != 4 || sink.rows[2][2] != "<NULL>" || sink.rows[0][2] != "12.34" || sink.rows[3][1] != "delta" || sink.rows[0][5] != "40000" || len(sink.rows[0][6]) != 40000 {
		t.Fatalf("unexpected rows")
	}
	q.Close()
	exec("DROP TABLE ttc_live_test PURGE")
}
