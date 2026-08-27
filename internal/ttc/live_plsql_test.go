package ttc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gizmodata/adbc-driver-oracle/internal/oratype"
)

func TestLiveOutBinds(t *testing.T) {
	ctx := context.Background()
	c, err := Dial(ctx, liveConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st := c.Prepare("BEGIN :out := :in * 2; :s := 'hello ' || :s; :d := SYSDATE; :n := NULL; END;")
	binds := []BindColumn{
		{OraTypeNum: TypeNumber, BufferSize: 22, Value: func(int) ([]byte, error) { return nil, nil }},
		{OraTypeNum: TypeNumber, BufferSize: 22, Value: func(int) ([]byte, error) { return oratype.EncodeInt64(nil, 21) }},
		{OraTypeNum: TypeVarchar, CSForm: CSFormImplicit, BufferSize: 5, Value: func(int) ([]byte, error) { return []byte("world"), nil }},
		{OraTypeNum: TypeDate, BufferSize: 7, Value: func(int) ([]byte, error) { return nil, nil }},
		{OraTypeNum: TypeVarchar, CSForm: CSFormImplicit, BufferSize: 1, Value: func(int) ([]byte, error) { return []byte("x"), nil }},
	}
	if err := st.Execute(ctx, ExecuteOptions{Binds: binds}); err != nil {
		t.Fatal(err)
	}
	t.Logf("dirs=%v", st.OutBindDirs())
	v, null := st.OutBindValue(0)
	d, _ := oratype.DecodeNumber(v, nil)
	if null || string(d.Text) != "42" {
		t.Fatalf("out bind 0: %q null=%v", d.Text, null)
	}
	v, null = st.OutBindValue(2)
	if null || string(v) != "hello world" {
		t.Fatalf("in/out bind 2: %q", v)
	}
	if _, null = st.OutBindValue(3); null {
		t.Fatal("date out bind is null")
	}
	if _, null = st.OutBindValue(4); !null {
		t.Fatal("expected NULL out bind")
	}
	st.Close()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLiveImplicitResult(t *testing.T) {
	ctx := context.Background()
	c, err := Dial(ctx, liveConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st := c.Prepare(`DECLARE c SYS_REFCURSOR; BEGIN
		OPEN c FOR SELECT LEVEL AS n, 'r' || LEVEL AS s FROM DUAL CONNECT BY LEVEL <= 2500;
		DBMS_SQL.RETURN_RESULT(c); END;`)
	if err := st.Execute(ctx, ExecuteOptions{}); err != nil {
		t.Fatal(err)
	}
	child := st.ImplicitResult()
	if child == nil {
		t.Fatal("no implicit result")
	}
	t.Logf("child cursor %d cols %v", child.cursorID, child.Columns())
	sink := &rowCollector{}
	ds := &deferredSink{inner: sink, stmt: child}
	for child.MoreRows() {
		if err := child.Fetch(ctx, ds, 1000); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.rows) != 2500 || sink.rows[2499][1] != "r2500" {
		t.Fatalf("rows=%d last=%v", len(sink.rows), sink.rows[len(sink.rows)-1])
	}
	child.Close()
	st.Close()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLiveCancel(t *testing.T) {
	ctx := context.Background()
	c, err := Dial(ctx, liveConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st := c.Prepare("SELECT COUNT(*) FROM (SELECT LEVEL FROM DUAL CONNECT BY LEVEL <= 30000) a, (SELECT LEVEL FROM DUAL CONNECT BY LEVEL <= 30000) b")
	tctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = st.Execute(tctx, ExecuteOptions{Sink: &deferredSink{inner: &rowCollector{}, stmt: st}})
	t.Logf("after %v: %v", time.Since(start), err)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancellation took too long")
	}
	st.Close()
	// connection must still be usable
	q := c.Prepare("SELECT 'ok' FROM DUAL")
	sink := &rowCollector{}
	if err := q.Execute(ctx, ExecuteOptions{Sink: &deferredSink{inner: sink, stmt: q}}); err != nil || !strings.Contains(sink.rows[0][0], "ok") {
		t.Fatalf("post-cancel query: %v %v", err, sink.rows)
	}
}
