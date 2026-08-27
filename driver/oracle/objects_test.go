package oracle

import (
	"context"

	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/apache/arrow-adbc/go/adbc"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/stretchr/testify/require"
)

func TestObjectTypesAsJSON(t *testing.T) {
	conn := openConn(t, map[string]string{OptionUseExtensionTypes: "true"})
	ctx := context.Background()
	run := func(sql string, must bool) {
		st, _ := conn.NewStatement()
		_ = st.SetSqlQuery(sql)
		_, err := st.ExecuteUpdate(ctx)
		st.Close()
		if must {
			require.NoError(t, err, sql)
		}
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	addr, list, person, table := "ADBC_ADDR_"+suffix, "ADBC_ADDRS_"+suffix, "ADBC_PERSON_"+suffix, "ADBC_OBJ_"+suffix
	run("CREATE TYPE "+addr+" AS OBJECT (street VARCHAR2(40), zip NUMBER(5), since DATE)", true)
	run("CREATE TYPE "+list+" AS VARRAY(5) OF "+addr, true)
	run("CREATE TYPE "+person+" AS OBJECT (name VARCHAR2(30), age NUMBER, home "+addr+", others "+list+", tags SYS.ODCIVARCHAR2LIST, score BINARY_DOUBLE, flag BOOLEAN, blob_attr BLOB)", true)
	run("CREATE TABLE "+table+" (id NUMBER, p "+person+", x XMLTYPE)", true)
	defer func() {
		run("DROP TABLE "+table+" PURGE", false)
		run("DROP TYPE "+person+" FORCE", false)
		run("DROP TYPE "+list+" FORCE", false)
		run("DROP TYPE "+addr+" FORCE", false)
	}()
	run("INSERT INTO "+table+" VALUES (1, "+person+"('Ann \"A\"', 41, "+addr+"('1 Main St', 12345, DATE '2020-01-02'), "+
		list+"("+addr+"('2 Side St', 54321, NULL), NULL), SYS.ODCIVARCHAR2LIST('a', 'b'), 2.5, TRUE, HEXTORAW('CAFE')), XMLTYPE('<doc><a>1</a></doc>'))", true)
	run("INSERT INTO "+table+" VALUES (2, NULL, NULL)", true)

	st, _ := conn.NewStatement()
	defer st.Close()
	require.NoError(t, st.SetSqlQuery("SELECT id, p, x FROM "+table+" ORDER BY id"))
	rr, _, err := st.ExecuteQuery(ctx)
	require.NoError(t, err)
	recs := readAll(t, rr)
	require.Len(t, recs, 1)
	rec := recs[0]
	defer rec.Release()
	t.Log(rec)
	require.EqualValues(t, 2, rec.NumRows())
	ext, _ := rec.Schema().Field(1).Metadata.GetValue("ARROW:extension:name")
	require.Equal(t, "arrow.json", ext)
	ext, _ = rec.Schema().Field(2).Metadata.GetValue("ARROW:extension:name")
	require.Equal(t, "arrow.opaque", ext)
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(rec.Column(1).(*array.String).Value(0)), &v))
	require.Equal(t, `Ann "A"`, v["NAME"])
	require.EqualValues(t, 41, v["AGE"])
	require.Equal(t, "1 Main St", v["HOME"].(map[string]any)["STREET"])
	require.Equal(t, "2020-01-02T00:00:00", v["HOME"].(map[string]any)["SINCE"])
	require.Len(t, v["OTHERS"], 2)
	require.Nil(t, v["OTHERS"].([]any)[1])
	require.Equal(t, []any{"a", "b"}, v["TAGS"])
	require.Equal(t, 2.5, v["SCORE"])
	require.Equal(t, true, v["FLAG"])
	require.Equal(t, "cafe", v["BLOB_ATTR"])
	require.Contains(t, rec.Column(2).(*array.String).Value(0), "<a>1</a>")
	require.True(t, rec.Column(1).IsNull(1))
	require.True(t, rec.Column(2).IsNull(1))
}

func TestNestedIngest(t *testing.T) {
	conn := openConn(t, nil)
	ctx := context.Background()
	table := fmt.Sprintf("ADBC_NEST_%d", time.Now().UnixNano()%100000)
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "tags", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true},
		{Name: "meta", Type: arrow.StructOf(arrow.Field{Name: "k", Type: arrow.BinaryTypes.String}, arrow.Field{Name: "n", Type: arrow.PrimitiveTypes.Int32}), Nullable: true},
	}, nil)
	rec, _, err := array.RecordFromJSON(memory.DefaultAllocator, schema, strings.NewReader(`[{"id": 1, "tags": ["a","b"], "meta": {"k": "x", "n": 7}}, {"id": 2, "tags": null, "meta": null}]`))
	require.NoError(t, err)
	defer rec.Release()
	stmt, _ := conn.NewStatement()
	require.NoError(t, stmt.SetOption(adbc.OptionKeyIngestTargetTable, table))
	require.NoError(t, stmt.Bind(ctx, rec))
	n, err := stmt.ExecuteUpdate(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	stmt.Close()
	defer func() {
		d, _ := conn.NewStatement()
		_ = d.SetSqlQuery("DROP TABLE " + table + " PURGE")
		_, _ = d.ExecuteUpdate(ctx)
		d.Close()
	}()
	q, _ := conn.NewStatement()
	defer q.Close()
	require.NoError(t, q.SetSqlQuery("SELECT id, tags, meta FROM "+table+" ORDER BY id"))
	rr, _, err := q.ExecuteQuery(ctx)
	require.NoError(t, err)
	recs := readAll(t, rr)
	out := recs[0]
	defer out.Release()
	t.Log(out.Schema(), out)
	require.Equal(t, `["a","b"]`, out.Column(1).(*array.String).Value(0))
	require.JSONEq(t, `{"k":"x","n":7}`, out.Column(2).(*array.String).Value(0))
	require.True(t, out.Column(1).IsNull(1))
}

func TestSDOGeometryWKB(t *testing.T) {
	uri := os.Getenv("ADBC_ORACLE_SPATIAL_URI")
	if uri == "" {
		t.Skip("ADBC_ORACLE_SPATIAL_URI not set (needs an Oracle with Spatial, e.g. gvenzl/oracle-free:23-faststart)")
	}
	drv := NewDriver(memory.DefaultAllocator)
	db, err := drv.NewDatabase(map[string]string{"uri": uri, OptionUseExtensionTypes: "true"})
	require.NoError(t, err)
	conn, err := db.Open(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	ctx := context.Background()
	table := fmt.Sprintf("ADBC_GEO_%d", time.Now().UnixNano()%100000)
	run := func(sql string) {
		st, _ := conn.NewStatement()
		_ = st.SetSqlQuery(sql)
		_, err := st.ExecuteUpdate(ctx)
		st.Close()
		require.NoError(t, err, sql)
	}
	run("CREATE TABLE " + table + " (id NUMBER, g MDSYS.SDO_GEOMETRY)")
	defer run("DROP TABLE " + table + " PURGE")
	run("INSERT INTO " + table + " VALUES (1, SDO_GEOMETRY(2001, 4326, SDO_POINT_TYPE(-122.4, 37.8, NULL), NULL, NULL))")
	run("INSERT INTO " + table + " VALUES (2, SDO_GEOMETRY(2003, NULL, NULL, SDO_ELEM_INFO_ARRAY(1,1003,1), SDO_ORDINATE_ARRAY(0,0, 10,0, 10,10, 0,10, 0,0)))")
	run("INSERT INTO " + table + " VALUES (3, SDO_GEOMETRY(2002, NULL, NULL, SDO_ELEM_INFO_ARRAY(1,2,1), SDO_ORDINATE_ARRAY(1,1, 2,2, 3,1)))")
	run("INSERT INTO " + table + " VALUES (4, SDO_GEOMETRY(2003, NULL, NULL, SDO_ELEM_INFO_ARRAY(1,1003,3), SDO_ORDINATE_ARRAY(0,0, 5,5)))")
	run("INSERT INTO " + table + " VALUES (5, SDO_GEOMETRY(3001, NULL, SDO_POINT_TYPE(1, 2, 3), NULL, NULL))")
	run("INSERT INTO " + table + " VALUES (6, NULL)")
	st, _ := conn.NewStatement()
	defer st.Close()
	require.NoError(t, st.SetSqlQuery("SELECT id, g FROM "+table+" ORDER BY id"))
	rr, _, err := st.ExecuteQuery(ctx)
	require.NoError(t, err)
	recs := readAll(t, rr)
	rec := recs[0]
	defer rec.Release()
	ext, _ := rec.Schema().Field(1).Metadata.GetValue("ARROW:extension:name")
	require.Equal(t, "geoarrow.wkb", ext)
	g := rec.Column(1).(*array.Binary)
	t.Log(hex.EncodeToString(g.Value(0)))
	// POINT(-122.4 37.8) little-endian WKB
	require.Equal(t, "0101000000"+"9a99999999995ec0"+"6666666666e64240", hex.EncodeToString(g.Value(0)))
	// POLYGON with one 5-point ring
	require.True(t, strings.HasPrefix(hex.EncodeToString(g.Value(1)), "0103000000"+"01000000"+"05000000"))
	require.True(t, strings.HasPrefix(hex.EncodeToString(g.Value(2)), "0102000000"+"03000000"))
	require.True(t, strings.HasPrefix(hex.EncodeToString(g.Value(3)), "0103000000"+"01000000"+"05000000"))
	require.True(t, strings.HasPrefix(hex.EncodeToString(g.Value(4)), "01e9030000")) // POINT Z = 1001
	require.True(t, g.IsNull(5))
}
