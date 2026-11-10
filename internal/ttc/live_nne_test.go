package ttc

import (
	"context"
	"strings"
	"testing"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
)

func TestLiveNNE(t *testing.T) {
	// An Oracle whose listener mandates NNE (scripts/setup_nne_server.sh;
	// CI's oracle_nne service). Skips when nothing listens at the default and
	// ADBC_ORACLE_NNE_URI is unset.
	cfg := liveServerConfig(t, "ADBC_ORACLE_NNE_URI", "oracle://scott:tiger@localhost:1523/FREEPDB1")
	cfg.ANO = &tns.ANOConfig{EncryptionLevel: tns.LevelRequired, ChecksumLevel: tns.LevelRequired}
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

// TestLiveNNEClientDriven verifies the client-side levels against a
// server that does NOT require NNE (the standard test container, whose
// sqlnet defaults are ACCEPTED): "required" and "requested" must
// negotiate encryption even though the server never demands it — the
// regression fixed here was "required" silently proceeding in cleartext.
func TestLiveNNEClientDriven(t *testing.T) {
	for _, lvl := range []struct {
		name  string
		level int
	}{{"required", tns.LevelRequired}, {"requested", tns.LevelRequested}} {
		t.Run(lvl.name, func(t *testing.T) {
			cfg := liveConfig(t)
			cfg.ANO = &tns.ANOConfig{EncryptionLevel: lvl.level, ChecksumLevel: lvl.level}
			ctx := context.Background()
			c, err := Dial(ctx, cfg)
			if err != nil {
				t.Fatalf("dial with NNE %s: %v", lvl.name, err)
			}
			defer c.Close()
			enc, chk, active := c.NNEInfo()
			if !active || enc == "" || chk == "" {
				t.Fatalf("expected negotiated encryption+checksum, got enc=%q chk=%q active=%v", enc, chk, active)
			}
			t.Logf("NNE active: encryption=%s checksum=%s", enc, chk)
			if err := c.Ping(ctx); err != nil {
				t.Fatalf("ping over NNE: %v", err)
			}
		})
	}
	// accepted (the default) keeps the proven plain path with this server.
	cfg := liveConfig(t)
	cfg.ANO = &tns.ANOConfig{EncryptionLevel: tns.LevelAccepted, ChecksumLevel: tns.LevelAccepted}
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dial with NNE accepted: %v", err)
	}
	defer c.Close()
	if _, _, active := c.NNEInfo(); active {
		t.Fatal("accepted level against a non-NNE server should stay unencrypted")
	}
}

// nneRejectedConfig points at an Oracle whose sqlnet.ora REJECTS NNE
// (SQLNET.ENCRYPTION_SERVER=REJECTED, CRYPTO_CHECKSUM_SERVER=REJECTED);
// scripts/setup_nne_server.sh makes one; CI runs it as the
// oracle_nne_rejected service. Skips when absent and unconfigured.
func nneRejectedConfig(t *testing.T) *Config {
	return liveServerConfig(t, "ADBC_ORACLE_NNE_REJECTED_URI", "oracle://scott:tiger@localhost:1524/FREEPDB1")
}

// TestLiveNNERejectedServer pins the fail-closed contract: against a
// server that rejects NNE, "required" must error (never silently
// proceed in cleartext) while "requested" and "accepted" connect
// unencrypted.
func TestLiveNNERejectedServer(t *testing.T) {
	ctx := context.Background()

	cfg := nneRejectedConfig(t)
	cfg.ANO = &tns.ANOConfig{EncryptionLevel: tns.LevelRequired, ChecksumLevel: tns.LevelRequired}
	if c, err := Dial(ctx, cfg); err == nil {
		c.Close()
		t.Fatal("NNE required against a rejecting server must fail closed")
	} else if !strings.Contains(err.Error(), "12660") {
		t.Fatalf("expected an ORA-12660 error, got: %v", err)
	} else {
		t.Logf("fail-closed error: %v", err)
	}

	for _, lvl := range []struct {
		name  string
		level int
	}{{"requested", tns.LevelRequested}, {"accepted", tns.LevelAccepted}} {
		cfg := nneRejectedConfig(t)
		cfg.ANO = &tns.ANOConfig{EncryptionLevel: lvl.level, ChecksumLevel: lvl.level}
		c, err := Dial(ctx, cfg)
		if err != nil {
			t.Fatalf("dial with NNE %s against rejecting server: %v", lvl.name, err)
		}
		if _, _, active := c.NNEInfo(); active {
			c.Close()
			t.Fatalf("NNE %s against a rejecting server should be unencrypted", lvl.name)
		}
		if err := c.Ping(ctx); err != nil {
			c.Close()
			t.Fatalf("ping (%s): %v", lvl.name, err)
		}
		c.Close()
	}
}
