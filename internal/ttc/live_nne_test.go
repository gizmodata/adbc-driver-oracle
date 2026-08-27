package ttc

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

func TestLiveNNE(t *testing.T) {
	host := "localhost"
	if h := os.Getenv("ORACLE_NNE_HOST"); h != "" {
		host = h
	}
	port := 1523
	if p := os.Getenv("ORACLE_NNE_PORT"); p != "" {
		port, _ = strconv.Atoi(p)
	}
	// Requires an Oracle whose listener mandates NNE (see the container
	// recipe in CLAUDE.md); skip when nothing is listening.
	probe, derr := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
	if derr != nil {
		t.Skipf("no NNE-required Oracle at %s:%d: %v", host, port, derr)
	}
	probe.Close()
	cfg := &Config{
		Addresses:   []Address{{Host: host, Port: port}},
		ServiceName: "FREEPDB1",
		Username:    "scott",
		Password:    "tiger",
		FullVersion: 1 << 20,
		ANO:         &tns.ANOConfig{EncryptionLevel: tns.LevelRequired, ChecksumLevel: tns.LevelRequired},
	}
	if os.Getenv("ORACLE_TRACE") != "" {
		cfg.Trace = func(f string, a ...any) { t.Logf(f, a...) }
	}
	ctx := context.Background()
	c, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("dial with NNE required: %v", err)
	}
	defer c.Close()
	sec := c.transport.Security()
	if !sec.Active() {
		t.Fatal("expected active packet encryption/checksum after connect")
	}
	t.Logf("NNE active: encryption=%d checksum=%d", sec.EncryptionID, sec.ChecksumID)

	// A real query proves every DATA packet encrypts/decrypts and the
	// checksum keystream stays in sync across many round trips.
	st := c.Prepare("SELECT LEVEL AS n, RPAD('x', 200, 'x') AS s FROM DUAL CONNECT BY LEVEL <= 5000")
	sink := &rowCollector{}
	ds := &deferredSink{inner: sink, stmt: st}
	if err := st.Execute(ctx, ExecuteOptions{PrefetchRows: 100, ArraySize: 500, Sink: ds}); err != nil {
		t.Fatal(err)
	}
	for st.MoreRows() {
		if err := st.Fetch(ctx, ds, 500); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.rows) != 5000 || sink.rows[4999][0] != "5000" {
		t.Fatalf("got %d rows, last %v", len(sink.rows), sink.rows[len(sink.rows)-1])
	}
	t.Logf("fetched %d encrypted rows", len(sink.rows))
	st.Close()

	// Error + recovery path exercises the marker/reset keystream re-init.
	bad := c.Prepare("SELECT * FROM no_such_table_nne")
	berr := bad.Execute(ctx, ExecuteOptions{})
	t.Logf("bad query error: %v", berr)
	if berr == nil {
		t.Fatal("expected error")
	}
	bad.Close()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping after error over NNE: %v", err)
	}
}
