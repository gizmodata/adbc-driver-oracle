package oracle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/stretchr/testify/require"

	"github.com/gizmodata/adbc-driver-oracle/internal/ttc"
)

// captureDeprecations sends the deprecation warnings to a buffer and forgets
// which keys were already warned about, for the length of the test.
func captureDeprecations(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := setDeprecationWriter(buf)
	deprecationWarned.Clear()
	t.Cleanup(func() {
		setDeprecationWriter(prev)
		deprecationWarned.Clear()
	})
	return buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func warningFor(oldKey string) string {
	return fmt.Sprintf("[oracle] option %q is deprecated; use %q\n", oldKey, DeprecatedOptions[oldKey])
}

func TestOptionNamesFollowStyleGuide(t *testing.T) {
	require.Len(t, DeprecatedOptions, 44)
	seen := map[string]string{}
	for oldKey, newKey := range DeprecatedOptions {
		require.True(t, strings.HasPrefix(oldKey, "adbc.oracle."), oldKey)
		require.True(t, strings.HasPrefix(newKey, "oracle."), "%s -> %s", oldKey, newKey)
		require.NotContains(t, newKey, ".sql.", newKey)
		other, dup := seen[newKey]
		require.False(t, dup, "%s and %s both map to %s", oldKey, other, newKey)
		seen[newKey] = oldKey
	}
	// Spec-defined keys keep their names.
	for _, key := range []string{OptionURI, OptionUsername, OptionPassword, OptionIngestTable} {
		_, renamed := DeprecatedOptions[key]
		require.False(t, renamed, key)
		require.Equal(t, key, canonicalOption(key))
	}
}

// Every old database-level key configures exactly what its new name does.
func TestDeprecatedDatabaseOptionsSetTheSameThing(t *testing.T) {
	captureDeprecations(t)
	base := map[string]string{OptionURI: "oracle://scott:tiger@127.0.0.1:1521/FREEPDB1"}
	missing := filepath.Join(t.TempDir(), "missing")
	cases := map[string]string{
		"adbc.oracle.host":                      "db.example.invalid",
		"adbc.oracle.port":                      "1600",
		"adbc.oracle.service_name":              "OTHERPDB",
		"adbc.oracle.sid":                       "ORCL",
		"adbc.oracle.tls":                       "true",
		"adbc.oracle.tls.ca_cert":               missing,
		"adbc.oracle.tls.skip_verify":           "true",
		"adbc.oracle.tls.server_name":           "cert.example.invalid",
		"adbc.oracle.wallet_location":           missing,
		"adbc.oracle.wallet_password":           "wallet-secret",
		"adbc.oracle.token":                     "token-secret",
		"adbc.oracle.mode":                      "sysdba",
		"adbc.oracle.allow_10g_verifier":        "true",
		"adbc.oracle.connect_timeout":           "7",
		"adbc.oracle.batch_size":                "10",
		"adbc.oracle.batch_bytes":               "1000",
		"adbc.oracle.prefetch_rows":             "5",
		"adbc.oracle.number_mode":               "decimal",
		"adbc.oracle.interval_mode":             "string",
		"adbc.oracle.date_mode":                 "date32",
		"adbc.oracle.use_extension_types":       "true",
		"adbc.oracle.session_time_zone":         "+02:00",
		"adbc.oracle.application_name":          "an-app",
		"adbc.oracle.current_schema":            "HR",
		"adbc.oracle.nne":                       "required",
		"adbc.oracle.nne_checksum":              "rejected",
		"adbc.oracle.nne_encryption_algorithms": "AES256",
		"adbc.oracle.nne_checksum_algorithms":   "SHA512",
		"adbc.oracle.disable_oob":               "true",
		"adbc.oracle.sdu":                       "8192",
		"adbc.oracle.trace":                     "true",
	}
	// Keys that only take effect together with another option.
	needs := map[string]map[string]string{
		"adbc.oracle.tls.ca_cert":     {OptionTLS: "true"},
		"adbc.oracle.tls.skip_verify": {OptionTLS: "true"},
		"adbc.oracle.tls.server_name": {OptionTLS: "true"},
	}
	baseCfg, err := parseOptions(base)
	require.NoError(t, err)
	for oldKey, value := range cases {
		t.Run(oldKey, func(t *testing.T) {
			newKey := DeprecatedOptions[oldKey]
			require.NotEmpty(t, newKey)
			build := func(key string) (*connConfig, error) {
				opts := map[string]string{key: value}
				for k, v := range base {
					opts[k] = v
				}
				for k, v := range needs[oldKey] {
					opts[k] = v
				}
				require.NoError(t, checkDatabaseOption(key))
				return parseOptions(opts)
			}
			viaOld, errOld := build(oldKey)
			viaNew, errNew := build(newKey)
			if errNew != nil {
				require.Error(t, errOld)
				require.Equal(t, errNew.Error(), errOld.Error())
				return
			}
			require.NoError(t, errOld)
			require.True(t, reflect.DeepEqual(viaNew, viaOld), "%s and %s configure different things", oldKey, newKey)
			if oldKey != "adbc.oracle.wallet_password" { // no effect without a wallet
				require.False(t, reflect.DeepEqual(baseCfg, viaNew), "%s had no effect", newKey)
			}
		})
	}
}

// Connection-level keys are refused at the database level under both names,
// and set the same thing on a connection.
func TestDeprecatedConnectionOptions(t *testing.T) {
	out := captureDeprecations(t)
	for _, oldKey := range []string{"adbc.oracle.module", "adbc.oracle.action", "adbc.oracle.client_info", "adbc.oracle.client_identifier", "adbc.oracle.nne_active", "adbc.oracle.nne_algorithms"} {
		for _, key := range []string{oldKey, DeprecatedOptions[oldKey]} {
			var ae adbc.Error
			require.True(t, errors.As(checkDatabaseOption(key), &ae), key)
			require.Equal(t, adbc.StatusNotImplemented, ae.Code, key)
		}
	}

	cfg, err := parseOptions(map[string]string{OptionURI: "oracle://scott:tiger@127.0.0.1:1521/FREEPDB1"})
	require.NoError(t, err)
	c := &connectionImpl{cfg: cfg, conn: &ttc.Conn{}}
	for _, oldKey := range []string{"adbc.oracle.module", "adbc.oracle.action", "adbc.oracle.client_info", "adbc.oracle.client_identifier"} {
		require.NoError(t, c.SetOption(oldKey, "x"), oldKey)
		require.NoError(t, c.SetOption(DeprecatedOptions[oldKey], "y"), oldKey)
	}
	settable := map[string][2]string{ // old key: value via the old name, via the new name
		"adbc.oracle.batch_size":          {"11", "12"},
		"adbc.oracle.prefetch_rows":       {"13", "14"},
		"adbc.oracle.batch_bytes":         {"15", "16"},
		"adbc.oracle.number_mode":         {"string", "decimal"},
		"adbc.oracle.interval_mode":       {"duration", "string"},
		"adbc.oracle.date_mode":           {"date32", "timestamp"},
		"adbc.oracle.use_extension_types": {adbc.OptionValueEnabled, adbc.OptionValueDisabled},
	}
	for oldKey, values := range settable {
		newKey := DeprecatedOptions[oldKey]
		require.NoError(t, c.SetOption(oldKey, values[0]))
		for _, key := range []string{oldKey, newKey} {
			got, err := c.GetOption(key)
			require.NoError(t, err)
			require.Equal(t, values[0], got, "%s after setting %s", key, oldKey)
		}
		require.NoError(t, c.SetOption(newKey, values[1]))
		for _, key := range []string{oldKey, newKey} {
			got, err := c.GetOption(key)
			require.NoError(t, err)
			require.Equal(t, values[1], got, "%s after setting %s", key, newKey)
		}
	}
	for _, oldKey := range []string{"adbc.oracle.nne_active", "adbc.oracle.nne_algorithms"} {
		viaOld, err := c.GetOption(oldKey)
		require.NoError(t, err)
		viaNew, err := c.GetOption(DeprecatedOptions[oldKey])
		require.NoError(t, err)
		require.Equal(t, viaNew, viaOld)
	}
	require.Contains(t, out.String(), warningFor("adbc.oracle.module"))
	require.Contains(t, out.String(), warningFor("adbc.oracle.nne_active"))
}

func TestDeprecatedStatementOptions(t *testing.T) {
	captureDeprecations(t)
	cases := map[string][2]string{
		"adbc.oracle.ingest.batch_rows":     {"100", "200"},
		"adbc.oracle.ingest.varchar_length": {"100", "200"},
		"adbc.oracle.ingest.raw_length":     {"100", "200"},
		"adbc.oracle.ingest.string_type":    {"CLOB", "NVARCHAR2"},
		"adbc.oracle.ingest.binary_type":    {"BLOB", "RAW"},
		"adbc.oracle.ingest.struct_type":    {"CLOB", "JSON"},
		"adbc.oracle.ingest.tablespace":     {"USERS", "SYSAUX"},
	}
	for oldKey, values := range cases {
		newKey := DeprecatedOptions[oldKey]
		s := &statementImpl{}
		require.NoError(t, s.SetOption(oldKey, values[0]))
		for _, key := range []string{oldKey, newKey} {
			got, err := s.GetOption(key)
			require.NoError(t, err)
			require.Equal(t, values[0], got, "%s after setting %s", key, oldKey)
		}
		require.NoError(t, s.SetOption(newKey, values[1]))
		for _, key := range []string{oldKey, newKey} {
			got, err := s.GetOption(key)
			require.NoError(t, err)
			require.Equal(t, values[1], got, "%s after setting %s", key, newKey)
		}
	}
	// Unset ingest options report their defaults.
	s := &statementImpl{}
	for key, want := range map[string]string{
		OptionIngestBatchRows: "5000", OptionIngestVarcharLength: "4000", OptionIngestRawLength: "2000",
		OptionIngestStringType: "VARCHAR2", OptionIngestBinaryType: "RAW", OptionIngestStructType: "", OptionIngestTablespace: "",
	} {
		got, err := s.GetOption(key)
		require.NoError(t, err)
		require.Equal(t, want, got, key)
	}
}

func TestDeprecatedDatabaseGetOption(t *testing.T) {
	captureDeprecations(t)
	drv := NewDriver(memory.DefaultAllocator)
	db, err := drv.NewDatabase(map[string]string{
		OptionURI:                "oracle://scott:tiger@127.0.0.1:1521/FREEPDB1",
		"adbc.oracle.batch_size": "123",
	})
	require.NoError(t, err)
	opts := db.(adbc.GetSetOptions)
	for _, key := range []string{"adbc.oracle.batch_size", OptionBatchSize} {
		got, err := opts.GetOption(key)
		require.NoError(t, err)
		require.Equal(t, "123", got, key)
	}
	require.NoError(t, opts.SetOption(OptionNumberType, "string"))
	for _, key := range []string{"adbc.oracle.number_mode", OptionNumberType} {
		got, err := opts.GetOption(key)
		require.NoError(t, err)
		require.Equal(t, "string", got, key)
	}
	// Secrets stay write-only under both names.
	for _, oldKey := range []string{"adbc.oracle.wallet_password", "adbc.oracle.token"} {
		require.NoError(t, opts.SetOption(oldKey, "s3cret"))
		for _, key := range []string{oldKey, DeprecatedOptions[oldKey]} {
			_, err := opts.GetOption(key)
			var ae adbc.Error
			require.True(t, errors.As(err, &ae), key)
			require.Equal(t, adbc.StatusNotFound, ae.Code, key)
			require.NotContains(t, ae.Msg, "s3cret")
		}
	}
}

// The new name wins when both are given in one call (whatever the map order);
// across separate SetOption calls the last write wins.
func TestNewOptionNameTakesPrecedence(t *testing.T) {
	captureDeprecations(t)
	uri := "oracle://scott:tiger@127.0.0.1:1521/FREEPDB1"
	cfg, err := parseOptions(map[string]string{
		OptionURI:                 uri,
		"adbc.oracle.batch_size":  "1",
		OptionBatchSize:           "2",
		"adbc.oracle.number_mode": "string",
		OptionNumberType:          "decimal",
	})
	require.NoError(t, err)
	require.Equal(t, 2, cfg.batchSize)
	require.Equal(t, NumberModeDecimal, cfg.types.numberMode)

	drv := NewDriver(memory.DefaultAllocator)
	for _, order := range [][2]string{{"adbc.oracle.batch_size", OptionBatchSize}, {OptionBatchSize, "adbc.oracle.batch_size"}} {
		db, err := drv.NewDatabase(map[string]string{OptionURI: uri})
		require.NoError(t, err)
		opts := db.(adbc.GetSetOptions)
		values := map[string]string{"adbc.oracle.batch_size": "1", OptionBatchSize: "2"}
		for _, key := range order {
			require.NoError(t, opts.SetOption(key, values[key]))
		}
		last := values[order[1]]
		for _, key := range []string{"adbc.oracle.batch_size", OptionBatchSize} {
			got, err := opts.GetOption(key)
			require.NoError(t, err)
			require.Equal(t, last, got, "%s (set in order %v)", key, order)
		}
		d := db.(*databaseImpl)
		cfg, err := parseOptions(d.opts)
		require.NoError(t, err)
		require.Equal(t, last, strconv.Itoa(cfg.batchSize), "order %v", order)
	}
}

// Each old key warns once per process, also when used from many goroutines,
// and the warning names the keys only, never the value.
func TestDeprecationWarningOncePerKey(t *testing.T) {
	out := captureDeprecations(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				require.Equal(t, OptionBatchSize, canonicalOption("adbc.oracle.batch_size"))
				require.Equal(t, OptionToken, canonicalOption("adbc.oracle.token"))
			}
		}()
	}
	wg.Wait()
	require.Equal(t, OptionBatchSize, canonicalOption(OptionBatchSize)) // new names never warn
	got := out.String()
	require.Equal(t, 1, strings.Count(got, warningFor("adbc.oracle.batch_size")), got)
	require.Equal(t, 1, strings.Count(got, warningFor("adbc.oracle.token")), got)
	require.Equal(t, 2, strings.Count(got, "\n"), got)
	require.Contains(t, got, `[oracle] option "adbc.oracle.batch_size" is deprecated; use "oracle.query.batch_size"`)
}

func TestDeprecationWarningNeverShowsTheValue(t *testing.T) {
	out := captureDeprecations(t)
	const secret = "pa55-W0rd-that-must-not-leak"
	drv := NewDriver(memory.DefaultAllocator)
	db, err := drv.NewDatabase(map[string]string{
		OptionURI:                     "oracle://scott@127.0.0.1:1521/FREEPDB1",
		"adbc.oracle.token":           secret,
		"adbc.oracle.wallet_password": secret,
	})
	require.NoError(t, err)
	opts := db.(adbc.GetSetOptions)
	require.NoError(t, opts.SetOption("adbc.oracle.tls.server_name", secret))
	_, err = parseOptions(db.(*databaseImpl).opts)
	require.NoError(t, err)
	got := out.String()
	require.Contains(t, got, warningFor("adbc.oracle.token"))
	require.Contains(t, got, warningFor("adbc.oracle.wallet_password"))
	require.Contains(t, got, warningFor("adbc.oracle.tls.server_name"))
	require.NotContains(t, got, secret)
	require.NotContains(t, got, "pa55")
}

func TestErrorMessagesCarryDriverPrefix(t *testing.T) {
	captureDeprecations(t)
	_, err := parseOptions(map[string]string{OptionURI: "oracle://scott@127.0.0.1:1521/FREEPDB1", OptionNumberType: "bogus"})
	var ae adbc.Error
	require.True(t, errors.As(err, &ae))
	require.Equal(t, `[oracle] unknown oracle.query.number_type "bogus"`, ae.Msg)

	require.True(t, errors.As(checkDatabaseOption("oracle.no_such_option"), &ae))
	require.Equal(t, `[oracle] unknown database option "oracle.no_such_option"`, ae.Msg)

	// Wire-layer messages keep their text (callers match it) and are never prefixed twice.
	require.Equal(t, "[oracle] oracle: boom", driverMsg("oracle: boom"))
	require.Equal(t, "[oracle] boom", driverMsg("[oracle] boom"))
	require.True(t, errors.As(fromTTCError(ttc.Err10gVerifierNotAllowed), &ae))
	require.True(t, strings.HasPrefix(ae.Msg, "[oracle] "), ae.Msg)
	require.Contains(t, ae.Msg, "oracle.auth.allow_10g_verifier=true")
}

// gizmodata/adbc-install's weekly canary (.github/scripts/check_driver_by_name.py) loads
// the driver by name and expects the connection error for a closed port to contain
// "oracle: cannot connect to 127.0.0.1:1": the [oracle] prefix may come first, but that
// text must stay as it is.
func TestConnectErrorKeepsTheTextTheInstallCanaryMatches(t *testing.T) {
	drv := NewDriver(memory.DefaultAllocator)
	db, err := drv.NewDatabase(map[string]string{OptionURI: "oracle://user:pass@127.0.0.1:1/service"})
	require.NoError(t, err)
	_, err = db.Open(context.Background())
	var ae adbc.Error
	require.True(t, errors.As(err, &ae), "%v", err)
	require.Contains(t, ae.Msg, "oracle: cannot connect to 127.0.0.1:1")
	require.True(t, strings.HasPrefix(ae.Msg, "[oracle] oracle: cannot connect to 127.0.0.1:1"), ae.Msg)
	require.Equal(t, adbc.StatusIO, ae.Code, ae.Msg)
}

// An unknown key is refused under the exact spelling the caller wrote, at every level,
// and so is an old key used at a level that doesn't take it.
func TestUnknownOptionsAreRefusedAsWritten(t *testing.T) {
	captureDeprecations(t)
	cfg, err := parseOptions(map[string]string{OptionURI: "oracle://scott:tiger@127.0.0.1:1521/FREEPDB1"})
	require.NoError(t, err)
	c := &connectionImpl{cfg: cfg, conn: &ttc.Conn{}}
	s := &statementImpl{conn: c}
	drv := NewDriver(memory.DefaultAllocator)
	db, err := drv.NewDatabase(map[string]string{OptionURI: "oracle://scott:tiger@127.0.0.1:1521/FREEPDB1"})
	require.NoError(t, err)
	dbOpts := db.(adbc.GetSetOptions)

	check := func(err error, code adbc.Status, want string) {
		t.Helper()
		var ae adbc.Error
		require.True(t, errors.As(err, &ae), "%v", err)
		require.Equal(t, code, ae.Code, ae.Msg)
		require.Equal(t, want, ae.Msg)
	}
	for _, key := range []string{"adbc.oracle.no_such_option", "oracle.no_such_option", "adbc.oracle.batch_sizes"} {
		check(dbOpts.SetOption(key, "1"), adbc.StatusNotImplemented, fmt.Sprintf("[oracle] unknown database option %q", key))
		_, err := drv.NewDatabase(map[string]string{OptionURI: "oracle://u@127.0.0.1:1/x", key: "1"})
		check(err, adbc.StatusNotImplemented, fmt.Sprintf("[oracle] unknown database option %q", key))
		_, err = dbOpts.GetOption(key)
		check(err, adbc.StatusNotFound, fmt.Sprintf("[oracle] unknown database option %q", key))
		check(c.SetOption(key, "1"), adbc.StatusNotImplemented, fmt.Sprintf("[oracle] unknown connection option %q", key))
		_, err = c.GetOption(key)
		check(err, adbc.StatusNotFound, fmt.Sprintf("[oracle] unknown connection option %q", key))
		check(s.SetOption(key, "1"), adbc.StatusNotImplemented, fmt.Sprintf("[oracle] unknown statement option %q", key))
		_, err = s.GetOption(key)
		check(err, adbc.StatusNotFound, fmt.Sprintf("[oracle] unknown statement option %q", key))
	}
	// Old names valid elsewhere are refused as written, not under their new names.
	check(s.SetOption("adbc.oracle.batch_size", "1"), adbc.StatusNotImplemented, `[oracle] unknown statement option "adbc.oracle.batch_size"`)
	check(c.SetOption("adbc.oracle.ingest.batch_rows", "1"), adbc.StatusNotImplemented, `[oracle] unknown connection option "adbc.oracle.ingest.batch_rows"`)
	check(dbOpts.SetOption("adbc.oracle.module", "m"), adbc.StatusNotImplemented, `[oracle] unknown database option "adbc.oracle.module"`)
	check(c.SetOption("adbc.oracle.batch_size", "x"), adbc.StatusInvalidArgument, `[oracle] invalid value "x" for adbc.oracle.batch_size`)
}
