package oracle

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/stretchr/testify/require"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
	"github.com/gizmodata/adbc-driver-oracle/internal/ttc"
)

const reviewTestURI = "oracle://scott:tiger@127.0.0.1:1521/FREEPDB1"

// A later SetOption under an old name must take effect even when the new name
// was set earlier: separate calls are last-write-wins. Before 1.1.0's fix the
// old-name call returned nil and was ignored, so hardening settings applied
// through the old names silently did nothing.
func TestLaterSetOptionWinsUnderEitherName(t *testing.T) {
	captureDeprecations(t)
	drv := NewDriver(memory.DefaultAllocator)
	db, err := drv.NewDatabase(map[string]string{
		OptionURI:              reviewTestURI,
		OptionNNE:              "accepted",
		OptionTLS:              "true",
		OptionTLSSkipVerify:    "true",
		OptionAllow10gVerifier: "true",
	})
	require.NoError(t, err)
	opts := db.(adbc.GetSetOptions)
	for oldKey, value := range map[string]string{
		"adbc.oracle.nne":                "required",
		"adbc.oracle.tls.skip_verify":    "false",
		"adbc.oracle.allow_10g_verifier": "false",
	} {
		require.NoError(t, opts.SetOption(oldKey, value))
		for _, key := range []string{oldKey, DeprecatedOptions[oldKey]} {
			got, err := opts.GetOption(key)
			require.NoError(t, err)
			require.Equal(t, value, got, "%s after setting %s", key, oldKey)
		}
	}
	cfg, err := parseOptions(db.(*databaseImpl).opts)
	require.NoError(t, err)
	require.Equal(t, tns.LevelRequired, cfg.ttc.ANO.EncryptionLevel)
	require.False(t, cfg.ttc.TLS.InsecureSkipVerify)
	require.False(t, cfg.ttc.Allow10gVerifier)

	// And back the other way: the new name set after the old one wins too.
	require.NoError(t, opts.SetOption(OptionNNE, "requested"))
	got, err := opts.GetOption("adbc.oracle.nne")
	require.NoError(t, err)
	require.Equal(t, "requested", got)
	// Stored once, under the current name.
	_, stillOld := db.(*databaseImpl).opts["adbc.oracle.nne"]
	require.False(t, stillOld)
}

// Options applied one at a time as one batch (the C library's connection
// options set before Init) apply old names first, so the new name wins every
// time instead of whichever Go's map order put last.
func TestSortOptionKeysForApply(t *testing.T) {
	captureDeprecations(t)
	batch := map[string]string{
		"adbc.oracle.number_mode": "string", OptionNumberType: "decimal",
		OptionDateType: "date32", "adbc.oracle.date_mode": "timestamp",
		"adbc.connection.autocommit": "true",
	}
	first := SortOptionKeysForApply(batch)
	require.Equal(t, []string{"adbc.oracle.date_mode", "adbc.oracle.number_mode", "adbc.connection.autocommit", OptionDateType, OptionNumberType}, first)
	for range 200 {
		keys := SortOptionKeysForApply(batch)
		require.Equal(t, first, keys)
		cfg, err := parseOptions(map[string]string{OptionURI: reviewTestURI})
		require.NoError(t, err)
		c := &connectionImpl{cfg: cfg, conn: &ttc.Conn{}}
		for _, k := range keys {
			if strings.HasPrefix(k, "adbc.connection.") {
				continue
			}
			require.NoError(t, c.SetOption(k, batch[k]))
		}
		got, err := c.GetOption(OptionNumberType)
		require.NoError(t, err)
		require.Equal(t, "decimal", got)
		got, err = c.GetOption(OptionDateType)
		require.NoError(t, err)
		require.Equal(t, "date32", got)
	}
}

// Every database option can be given as a URI parameter by the last part of its
// name, except host and port, which belong in the URI's authority.
func TestURIParameterForEveryDatabaseOption(t *testing.T) {
	captureDeprecations(t)
	values := map[string]string{
		"connect_timeout_seconds": "5", "batch_size": "10", "batch_bytes": "10", "prefetch_rows": "10",
		"number_type": "decimal", "interval_type": "string", "date_type": "date32", "nne": "required",
		"checksum": "required", "sdu": "8192", "mode": "sysdba", "tls": "false", "skip_verify": "true",
		"allow_10g_verifier": "true", "use_extension_types": "true", "disable_oob": "true", "trace": "false",
		"encryption_algorithms": "AES256", "checksum_algorithms": "SHA512", "ca_cert": "/nonexistent",
	}
	for key := range databaseOptions {
		if !strings.HasPrefix(key, "oracle.") || key == OptionHost || key == OptionPort {
			continue
		}
		last := key[strings.LastIndex(key, ".")+1:]
		v := values[last]
		if v == "" {
			v = "x"
		}
		_, err := parseOptions(map[string]string{OptionURI: reviewTestURI + "?" + last + "=" + v})
		if err != nil {
			require.NotContains(t, err.Error(), "unknown URI parameter", "%s as ?%s=", key, last)
		}
	}
	for _, p := range []string{"host", "port"} {
		_, err := parseOptions(map[string]string{OptionURI: reviewTestURI + "?" + p + "=x"})
		require.ErrorContains(t, err, "unknown URI parameter")
	}
	// The aliases set the option.
	cfg, err := parseOptions(map[string]string{OptionURI: reviewTestURI + "?checksum=rejected&encryption_algorithms=AES256&checksum_algorithms=SHA512"})
	require.NoError(t, err)
	require.Equal(t, tns.LevelRejected, cfg.ttc.ANO.ChecksumLevel)
	require.Equal(t, []string{"AES256"}, cfg.ttc.ANO.Encryption)
	require.Equal(t, []string{"SHA512"}, cfg.ttc.ANO.Checksum)
	cfg, err = parseOptions(map[string]string{OptionURI: reviewTestURI + "?tls=true&skip_verify=true&server_name=cert.example.invalid"})
	require.NoError(t, err)
	require.True(t, cfg.ttc.TLS.InsecureSkipVerify)
	require.Equal(t, "cert.example.invalid", cfg.ttc.TLS.ServerName)
}

// Two spellings of one setting with different values are refused (naming the
// parameters, never the values); the same value twice is fine.
func TestURIParameterSpellingsMustAgree(t *testing.T) {
	for range 20 { // url.Values is a map: both iteration orders must be refused
		_, err := parseOptions(map[string]string{OptionURI: reviewTestURI + "?nne_checksum=required&checksum=rejected"})
		var ae adbc.Error
		require.True(t, errors.As(err, &ae), "%v", err)
		require.Equal(t, adbc.StatusInvalidArgument, ae.Code)
		require.Contains(t, ae.Msg, "set the same option to different values")
		require.NotContains(t, ae.Msg, "required")
		require.NotContains(t, ae.Msg, "rejected")
		_, err = parseOptions(map[string]string{OptionURI: "oracle://scott@127.0.0.1:1521/FREEPDB1?password=Sekr1t-A&pwd=Sekr1t-B"})
		require.ErrorContains(t, err, "set the same option to different values")
		require.NotContains(t, err.Error(), "Sekr1t")
	}
	_, err := parseOptions(map[string]string{OptionURI: reviewTestURI + "?nne_checksum=required&checksum=required"})
	require.NoError(t, err)
}

func TestParseTimeoutBounds(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"0": 0, "30": 30 * time.Second, "0.5": 500 * time.Millisecond, "1.5s": 1500 * time.Millisecond,
		"86400": 24 * time.Hour, "24h": 24 * time.Hour,
	} {
		got, err := parseTimeout(v)
		require.NoError(t, err, v)
		require.Equal(t, want, got, v)
	}
	for _, v := range []string{"1e10", "9.3e9", "1e300", "86401", "25h", "-1", "-1s", "NaN", "Inf", "+Inf", "abc", ""} {
		_, err := parseTimeout(v)
		var ae adbc.Error
		require.True(t, errors.As(err, &ae), "parseTimeout(%q) = %v", v, err)
		require.Equal(t, adbc.StatusInvalidArgument, ae.Code, v)
	}
}

// Errors about a URI or connect string never repeat it: it can hold a password.
func TestURIErrorsNeverQuoteCredentials(t *testing.T) {
	const password = "Qx7%zz@:/Kv9"
	fragments := func() []string {
		var out []string
		for i := 0; i+3 <= len(password); i++ {
			out = append(out, password[i:i+3])
		}
		return out
	}()
	uris := []string{
		"oracle://scott:" + password + "@db.example.invalid:1521/S",
		"oracle://scott:Qx7%zz@db.example.invalid:1521/S",
		"oracle://scott:Qx7Kv9@db.example.invalid:Kv9x/S",
		"oracle://scott:Qx7/Kv9@db.example.invalid:1521/S",
		"oracle://scott@db.example.invalid:1521/S?password=" + password,
		"oracle://scott@db.example.invalid:1521/S?x=%zz&password=Qx7Kv9",
		"db.example.invalid:1521/S?x=%zz&password=Qx7Kv9",
		"scott/" + password + "@db.example.invalid:1521/S",
		"scott/Qx7Kv9@db.example.invalid:1521/S",
	}
	for _, uri := range uris {
		_, err := parseOptions(map[string]string{OptionURI: uri, OptionUsername: "scott"})
		require.Error(t, err, uri)
		msg := err.Error()
		require.True(t, strings.HasPrefix(err.(adbc.Error).Msg, "[oracle] "), msg)
		for _, f := range append(fragments, "Qx7", "Kv9", "zz@", "%zz") {
			require.NotContains(t, msg, f, "error for %q: %s", uri, msg)
		}
		require.NotContains(t, msg, "db.example.invalid:1521/S", msg)
	}
	_, err := parseOptions(map[string]string{OptionURI: "oracle://scott:S3cret%zz@db:1521/S"})
	require.ErrorContains(t, err, "a % that doesn't start a two-digit hex escape (write a literal % as %25)")
}

// GetOption("uri") hides the password and secret parameters.
func TestGetOptionURIRedactsSecrets(t *testing.T) {
	cases := map[string]string{
		"oracle://scott:S3cretPW@127.0.0.1:1/FREEPDB1":                                             "oracle://scott:xxxxx@127.0.0.1:1/FREEPDB1",
		"oracle://scott:S3cret%2FPW@127.0.0.1:1/FREEPDB1?number_type=decimal":                      "oracle://scott:xxxxx@127.0.0.1:1/FREEPDB1?number_type=decimal",
		"oracle://scott@127.0.0.1:1/FREEPDB1?password=S3cretPW&tls=false":                          "oracle://scott@127.0.0.1:1/FREEPDB1?password=xxxxx&tls=false",
		"oracle://127.0.0.1:1/FREEPDB1?pwd=S3cretPW&token=T0k&wallet_password=W":                   "oracle://127.0.0.1:1/FREEPDB1?pwd=xxxxx&token=xxxxx&wallet_password=xxxxx",
		"127.0.0.1:1/FREEPDB1?password=S3cretPW":                                                   "127.0.0.1:1/FREEPDB1?password=xxxxx",
		"127.0.0.1:1/FREEPDB1":                                                                     "127.0.0.1:1/FREEPDB1",
		"scott/S3cretPW@127.0.0.1:1/FREEPDB1":                                                      "xxxxx",
		"oracle://scott:S3cret%zz@db:1521/S":                                                       "xxxxx",
		"(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=h)(PORT=1521))(CONNECT_DATA=(SERVICE_NAME=S)))": "(DESCRIPTION=(ADDRESS=(PROTOCOL=TCP)(HOST=h)(PORT=1521))(CONNECT_DATA=(SERVICE_NAME=S)))",
	}
	drv := NewDriver(memory.DefaultAllocator)
	for uri, want := range cases {
		db, err := drv.NewDatabase(map[string]string{OptionURI: uri})
		require.NoError(t, err)
		got, err := db.(adbc.GetSetOptions).GetOption(OptionURI)
		require.NoError(t, err)
		require.Equal(t, want, got, uri)
		require.NotContains(t, got, "S3cret")
	}
}

// Separate checks in one table so a failure names the case.
func TestNewDatabaseAndSetOptionsOneCallNewNameWins(t *testing.T) {
	captureDeprecations(t)
	drv := NewDriver(memory.DefaultAllocator)
	for range 50 {
		db, err := drv.NewDatabase(map[string]string{OptionURI: reviewTestURI, "adbc.oracle.batch_size": "1", OptionBatchSize: "2"})
		require.NoError(t, err)
		got, err := db.(adbc.GetSetOptions).GetOption("adbc.oracle.batch_size")
		require.NoError(t, err)
		require.Equal(t, "2", got)
		require.NoError(t, db.SetOptions(map[string]string{OptionNumberType: "decimal", "adbc.oracle.number_mode": "string"}))
		got, err = db.(adbc.GetSetOptions).GetOption(OptionNumberType)
		require.NoError(t, err)
		require.Equal(t, "decimal", got, fmt.Sprint("SetOptions with both names"))
	}
}
