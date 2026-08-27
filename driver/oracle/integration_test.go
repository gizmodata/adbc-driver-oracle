package oracle

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/stretchr/testify/require"
)

func testURI(t *testing.T) string {
	uri := os.Getenv("ADBC_ORACLE_TEST_URI")
	if uri == "" {
		uri = "oracle://scott:tiger@localhost:1521/FREEPDB1"
	}
	host := "localhost:1521"
	if i := strings.Index(uri, "@"); i > 0 {
		rest := uri[i+1:]
		if j := strings.Index(rest, "/"); j > 0 {
			host = rest[:j]
		}
	}
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		t.Skipf("no Oracle at %s: %v", host, err)
	}
	conn.Close()
	return uri
}

func openConn(t *testing.T, extra map[string]string) adbc.Connection {
	t.Helper()
	opts := map[string]string{adbc.OptionKeyURI: testURI(t)}
	for k, v := range extra {
		opts[k] = v
	}
	drv := NewDriver(memory.DefaultAllocator)
	db, err := drv.NewDatabase(opts)
	require.NoError(t, err)
	conn, err := db.Open(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readAll(t *testing.T, rr array.RecordReader) []arrow.Record {
	t.Helper()
	var out []arrow.Record
	for rr.Next() {
		rec := rr.Record()
		rec.Retain()
		out = append(out, rec)
	}
	require.NoError(t, rr.Err())
	rr.Release()
	return out
}

func TestQueryTypes(t *testing.T) {
	conn := openConn(t, nil)
	ctx := context.Background()
	stmt, err := conn.NewStatement()
	require.NoError(t, err)
	defer stmt.Close()
	require.NoError(t, stmt.SetSqlQuery(`SELECT 42 AS answer, 'hello' AS greeting, CAST(12.34 AS NUMBER(10,2)) AS dec,
		CAST(7 AS NUMBER(10)) AS i, SYSTIMESTAMP AS ts, DATE '2024-01-02' AS d, HEXTORAW('DEADBEEF') AS r, NULL AS n FROM DUAL`))
	rr, n, err := stmt.ExecuteQuery(ctx)
	require.NoError(t, err)
	require.EqualValues(t, -1, n)
	recs := readAll(t, rr)
	require.Len(t, recs, 1)
	rec := recs[0]
	t.Log(rec.Schema())
	t.Log(rec)
	require.EqualValues(t, 1, rec.NumRows())
	require.Equal(t, "ANSWER", rec.Schema().Field(0).Name)
	require.Equal(t, arrow.PrimitiveTypes.Float64, rec.Schema().Field(0).Type)
	require.Equal(t, arrow.BinaryTypes.String, rec.Schema().Field(1).Type)
	require.Equal(t, "decimal(10, 2)", rec.Schema().Field(2).Type.String())
	require.Equal(t, arrow.PrimitiveTypes.Int64, rec.Schema().Field(3).Type)
	require.Equal(t, "hello", rec.Column(1).(*array.String).Value(0))
	require.Equal(t, int64(7), rec.Column(3).(*array.Int64).Value(0))
	require.True(t, rec.Column(7).IsNull(0))
	rec.Release()
}

func TestStreamingAndBinds(t *testing.T) {
	conn := openConn(t, map[string]string{OptionBatchSize: "1000"})
	ctx := context.Background()
	stmt, err := conn.NewStatement()
	require.NoError(t, err)
	defer stmt.Close()
	require.NoError(t, stmt.SetSqlQuery("SELECT LEVEL AS n FROM DUAL CONNECT BY LEVEL <= ?"))
	params, _, err := array.RecordFromJSON(memory.DefaultAllocator, arrow.NewSchema([]arrow.Field{{Name: "0", Type: arrow.PrimitiveTypes.Int64}}, nil), strings.NewReader(`[{"0": 12345}]`))
	require.NoError(t, err)
	defer params.Release()
	require.NoError(t, stmt.Bind(ctx, params))
	rr, _, err := stmt.ExecuteQuery(ctx)
	require.NoError(t, err)
	recs := readAll(t, rr)
	total := int64(0)
	for _, r := range recs {
		require.LessOrEqual(t, r.NumRows(), int64(1000))
		total += r.NumRows()
		r.Release()
	}
	require.EqualValues(t, 12345, total)
	require.GreaterOrEqual(t, len(recs), 13)
}

func TestIngestRoundTrip(t *testing.T) {
	conn := openConn(t, nil)
	ctx := context.Background()
	table := fmt.Sprintf("ADBC_GO_%d", time.Now().UnixNano()%1000000)
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "amt", Type: &arrow.Decimal128Type{Precision: 10, Scale: 2}, Nullable: true},
		{Name: "f", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Microsecond}, Nullable: true},
		{Name: "b", Type: arrow.FixedWidthTypes.Boolean, Nullable: true},
	}, nil)
	rec, _, err := array.RecordFromJSON(memory.DefaultAllocator, schema, strings.NewReader(`[
		{"id": 1, "name": "one", "amt": "12.34", "f": 1.5, "ts": "2024-01-02T03:04:05.123456Z", "b": true},
		{"id": 2, "name": null, "amt": null, "f": null, "ts": null, "b": false},
		{"id": 3, "name": "three", "amt": "-0.01", "f": -2.25, "ts": "1999-12-31T23:59:59Z", "b": null}
	]`))
	require.NoError(t, err)
	defer rec.Release()

	stmt, err := conn.NewStatement()
	require.NoError(t, err)
	require.NoError(t, stmt.SetOption(adbc.OptionKeyIngestTargetTable, table))
	require.NoError(t, stmt.SetOption(adbc.OptionKeyIngestMode, adbc.OptionValueIngestModeCreate))
	require.NoError(t, stmt.Bind(ctx, rec))
	n, err := stmt.ExecuteUpdate(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, n)
	stmt.Close()

	q, err := conn.NewStatement()
	require.NoError(t, err)
	defer q.Close()
	require.NoError(t, q.SetSqlQuery("SELECT id, name, amt, f, ts, b FROM "+table+" ORDER BY id"))
	rr, _, err := q.ExecuteQuery(ctx)
	require.NoError(t, err)
	recs := readAll(t, rr)
	require.Len(t, recs, 1)
	out := recs[0]
	t.Log(out.Schema())
	t.Log(out)
	require.EqualValues(t, 3, out.NumRows())
	require.Equal(t, "one", out.Column(1).(*array.String).Value(0))
	require.True(t, out.Column(1).IsNull(1))
	require.Equal(t, "-0.01", out.Column(2).(*array.Decimal128).Value(2).ToString(2))
	require.Equal(t, -2.25, out.Column(3).(*array.Float64).Value(2))
	out.Release()

	drop, _ := conn.NewStatement()
	require.NoError(t, drop.SetSqlQuery("DROP TABLE "+table+" PURGE"))
	_, err = drop.ExecuteUpdate(ctx)
	require.NoError(t, err)
	drop.Close()
}

func TestMetadata(t *testing.T) {
	conn := openConn(t, nil)
	ctx := context.Background()
	rr, err := conn.GetInfo(ctx, nil)
	require.NoError(t, err)
	recs := readAll(t, rr)
	t.Log(recs[0])
	recs[0].Release()

	table := fmt.Sprintf("ADBC_GO_META_%d", time.Now().UnixNano()%1000000)
	st, _ := conn.NewStatement()
	require.NoError(t, st.SetSqlQuery("CREATE TABLE "+table+" (id NUMBER(10) PRIMARY KEY, v VARCHAR2(30) NOT NULL, d DATE)"))
	_, err = st.ExecuteUpdate(ctx)
	require.NoError(t, err)
	st.Close()
	defer func() {
		d, _ := conn.NewStatement()
		_ = d.SetSqlQuery("DROP TABLE " + table + " PURGE")
		_, _ = d.ExecuteUpdate(ctx)
		d.Close()
	}()

	sch, err := conn.GetTableSchema(ctx, nil, nil, table)
	require.NoError(t, err)
	t.Log(sch)
	require.Equal(t, []string{"ID", "V", "D"}, []string{sch.Field(0).Name, sch.Field(1).Name, sch.Field(2).Name})
	require.False(t, sch.Field(1).Nullable)

	rr, err = conn.GetObjects(ctx, adbc.ObjectDepthAll, nil, nil, &table, nil, nil)
	require.NoError(t, err)
	recs = readAll(t, rr)
	t.Log(recs[0])
	require.Contains(t, fmt.Sprint(recs[0]), table)
	require.Contains(t, fmt.Sprint(recs[0]), "PRIMARY KEY")
	recs[0].Release()
}

func TestTransactions(t *testing.T) {
	conn := openConn(t, nil)
	ctx := context.Background()
	table := fmt.Sprintf("ADBC_GO_TX_%d", time.Now().UnixNano()%1000000)
	exec := func(sql string) {
		st, _ := conn.NewStatement()
		require.NoError(t, st.SetSqlQuery(sql))
		_, err := st.ExecuteUpdate(ctx)
		require.NoError(t, err, sql)
		st.Close()
	}
	exec("CREATE TABLE " + table + " (id NUMBER(10))")
	defer exec("DROP TABLE " + table + " PURGE")
	require.NoError(t, conn.(adbc.PostInitOptions).SetOption(adbc.OptionKeyAutoCommit, adbc.OptionValueDisabled))
	exec("INSERT INTO " + table + " VALUES (1)")
	require.NoError(t, conn.Rollback(ctx))
	exec("INSERT INTO " + table + " VALUES (2)")
	require.NoError(t, conn.Commit(ctx))
	require.NoError(t, conn.(adbc.PostInitOptions).SetOption(adbc.OptionKeyAutoCommit, adbc.OptionValueEnabled))
	st, _ := conn.NewStatement()
	require.NoError(t, st.SetSqlQuery("SELECT id FROM "+table))
	rr, _, err := st.ExecuteQuery(ctx)
	require.NoError(t, err)
	recs := readAll(t, rr)
	require.Len(t, recs, 1)
	require.EqualValues(t, 1, recs[0].NumRows())
	require.EqualValues(t, 2, recs[0].Column(0).(*array.Int64).Value(0))
	recs[0].Release()
	st.Close()
}

func TestPLSQLOutBindsAndImplicitResults(t *testing.T) {
	conn := openConn(t, nil)
	ctx := context.Background()

	// OUT / IN OUT binds come back as a one-row result set.
	stmt, err := conn.NewStatement()
	require.NoError(t, err)
	defer stmt.Close()
	require.NoError(t, stmt.SetSqlQuery("BEGIN :doubled := :n * 2; :greeting := 'hello ' || :greeting; END;"))
	params, _, err := array.RecordFromJSON(memory.DefaultAllocator, arrow.NewSchema([]arrow.Field{
		{Name: "doubled", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "n", Type: arrow.PrimitiveTypes.Int64},
		{Name: "greeting", Type: arrow.BinaryTypes.String},
	}, nil), strings.NewReader(`[{"doubled": null, "n": 21, "greeting": "world"}]`))
	require.NoError(t, err)
	defer params.Release()
	require.NoError(t, stmt.Bind(ctx, params))
	rr, _, err := stmt.ExecuteQuery(ctx)
	require.NoError(t, err)
	recs := readAll(t, rr)
	require.Len(t, recs, 1)
	rec := recs[0]
	t.Log(rec.Schema(), rec)
	require.Equal(t, []string{"DOUBLED", "GREETING"}, []string{rec.Schema().Field(0).Name, rec.Schema().Field(1).Name})
	pt, _ := rec.Schema().Field(1).Metadata.GetValue("ORACLE:parameter_type")
	require.Equal(t, "IN OUT", pt)
	require.EqualValues(t, 42, rec.Column(0).(*array.Int64).Value(0))
	require.Equal(t, "hello world", rec.Column(1).(*array.String).Value(0))
	rec.Release()

	// Implicit result set from DBMS_SQL.RETURN_RESULT streams like a query.
	stmt2, err := conn.NewStatement()
	require.NoError(t, err)
	defer stmt2.Close()
	require.NoError(t, stmt2.SetSqlQuery(`DECLARE c SYS_REFCURSOR; BEGIN
		OPEN c FOR SELECT LEVEL AS n, 'r' || LEVEL AS s FROM DUAL CONNECT BY LEVEL <= 70000;
		DBMS_SQL.RETURN_RESULT(c); END;`))
	rr, _, err = stmt2.ExecuteQuery(ctx)
	require.NoError(t, err)
	recs = readAll(t, rr)
	total := int64(0)
	for _, r := range recs {
		total += r.NumRows()
		r.Release()
	}
	require.EqualValues(t, 70000, total)
	require.GreaterOrEqual(t, len(recs), 2)
}

func TestTypeModesAndBatchBytes(t *testing.T) {
	conn := openConn(t, map[string]string{
		OptionIntervalMode: "duration", OptionDateMode: "date32", OptionBatchBytes: "20000", OptionBatchSize: "100000",
	})
	ctx := context.Background()
	stmt, err := conn.NewStatement()
	require.NoError(t, err)
	defer stmt.Close()
	require.NoError(t, stmt.SetSqlQuery(`SELECT DATE '2024-01-02' AS d, INTERVAL '1 02:03:04.5' DAY TO SECOND AS ds,
		INTERVAL '2-3' YEAR TO MONTH AS ym, RPAD('x', 1000, 'x') AS pad FROM DUAL CONNECT BY LEVEL <= 500`))
	rr, _, err := stmt.ExecuteQuery(ctx)
	require.NoError(t, err)
	recs := readAll(t, rr)
	require.Greater(t, len(recs), 5, "batch_bytes should split 500 x 1KB rows into many batches")
	rec := recs[0]
	require.Equal(t, arrow.FixedWidthTypes.Date32, rec.Schema().Field(0).Type)
	require.Equal(t, "duration[us]", rec.Schema().Field(1).Type.String())
	require.Equal(t, arrow.FixedWidthTypes.MonthDayNanoInterval, rec.Schema().Field(2).Type)
	require.EqualValues(t, 19724, rec.Column(0).(*array.Date32).Value(0)) // 2024-01-02
	require.EqualValues(t, (26*3600+3*60+4)*1000000+500000, rec.Column(1).(*array.Duration).Value(0))
	for _, r := range recs {
		r.Release()
	}

	conn2 := openConn(t, map[string]string{OptionIntervalMode: "string"})
	st2, _ := conn2.NewStatement()
	defer st2.Close()
	require.NoError(t, st2.SetSqlQuery("SELECT INTERVAL '-1 02:03:04.5' DAY TO SECOND AS ds, INTERVAL '2-3' YEAR TO MONTH AS ym FROM DUAL"))
	rr, _, err = st2.ExecuteQuery(ctx)
	require.NoError(t, err)
	recs = readAll(t, rr)
	require.Equal(t, "-P1DT2H3M4.5S", recs[0].Column(0).(*array.String).Value(0))
	require.Equal(t, "P2Y3M", recs[0].Column(1).(*array.String).Value(0))
	recs[0].Release()
}

func TestCancelViaContext(t *testing.T) {
	conn := openConn(t, nil)
	stmt, err := conn.NewStatement()
	require.NoError(t, err)
	defer stmt.Close()
	require.NoError(t, stmt.SetSqlQuery("SELECT COUNT(*) FROM (SELECT LEVEL FROM DUAL CONNECT BY LEVEL <= 30000) a, (SELECT LEVEL FROM DUAL CONNECT BY LEVEL <= 30000) b"))
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = stmt.ExecuteQuery(ctx)
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second)
	var ae adbc.Error
	require.ErrorAs(t, err, &ae)
	require.Equal(t, adbc.StatusTimeout, ae.Code, err.Error())
	// connection still usable
	st2, _ := conn.NewStatement()
	defer st2.Close()
	require.NoError(t, st2.SetSqlQuery("SELECT 1 FROM DUAL"))
	rr, _, err := st2.ExecuteQuery(context.Background())
	require.NoError(t, err)
	readAll(t, rr)
}
