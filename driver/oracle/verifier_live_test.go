package oracle

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/stretchr/testify/require"
)

// Live tests for the password verifier types. Each needs an account whose only
// verifier is the one under test, which current Oracle releases no longer make
// by default (see CLAUDE.md, "Password verifiers"):
//
//	ADBC_ORACLE_TEST_10G_URI  account with only a 10g (O3LOGON) verifier
//	ADBC_ORACLE_TEST_11G_URI  account with only an 11g verifier
//	ADBC_ORACLE_TEST_12C_URI  account with a 12c verifier on the same server
//
// They skip when their URI is unset, unless ADBC_ORACLE_TEST_VERIFIERS_REQUIRED
// is true (CI sets it), which turns a missing URI into a failure.
func verifierURI(t *testing.T, env string) string {
	t.Helper()
	uri := os.Getenv(env)
	if uri == "" {
		if isTrue(os.Getenv("ADBC_ORACLE_TEST_VERIFIERS_REQUIRED")) {
			t.Fatalf("%s is not set, but ADBC_ORACLE_TEST_VERIFIERS_REQUIRED is", env)
		}
		t.Skipf("%s is not set", env)
	}
	return uri
}

// openVerifierConn opens a connection, returning the Open error instead of
// failing the test.
func openVerifierConn(t *testing.T, uri string, extra map[string]string) (adbc.Connection, error) {
	t.Helper()
	opts := map[string]string{adbc.OptionKeyURI: uri}
	for k, v := range extra {
		opts[k] = v
	}
	db, err := NewDriver(memory.DefaultAllocator).NewDatabase(opts)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	conn, err := db.Open(context.Background())
	if err == nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, err
}

// withPassword returns uri with its password replaced.
func withPassword(t *testing.T, uri, password string) string {
	t.Helper()
	u, err := url.Parse(uri)
	require.NoError(t, err)
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String()
}

func uriPassword(t *testing.T, uri string) string {
	t.Helper()
	u, err := url.Parse(uri)
	require.NoError(t, err)
	pw, _ := u.User.Password()
	return pw
}

// requireSessionUser checks that conn is a working session for user.
func requireSessionUser(t *testing.T, conn adbc.Connection, user string) {
	t.Helper()
	stmt, err := conn.NewStatement()
	require.NoError(t, err)
	defer stmt.Close()
	require.NoError(t, stmt.SetSqlQuery("SELECT USER FROM DUAL"))
	rr, _, err := stmt.ExecuteQuery(context.Background())
	require.NoError(t, err)
	recs := readAll(t, rr)
	require.Len(t, recs, 1)
	require.Equal(t, strings.ToUpper(user), recs[0].Column(0).(*array.String).Value(0))
}

func uriUser(t *testing.T, uri string) string {
	t.Helper()
	u, err := url.Parse(uri)
	require.NoError(t, err)
	return u.User.Username()
}

func requireADBCError(t *testing.T, err error) adbc.Error {
	t.Helper()
	require.Error(t, err)
	var ae adbc.Error
	require.True(t, errors.As(err, &ae), "not an adbc.Error: %v", err)
	return ae
}

func TestLive10gVerifierRefusedByDefault(t *testing.T) {
	uri := verifierURI(t, "ADBC_ORACLE_TEST_10G_URI")
	for name, extra := range map[string]map[string]string{
		"unset":    nil,
		"explicit": {OptionAllow10gVerifier: "false"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := openVerifierConn(t, uri, extra)
			ae := requireADBCError(t, err)
			require.Equal(t, adbc.StatusUnauthenticated, ae.Code, ae.Msg)
			require.EqualValues(t, 0, ae.VendorCode, "refused by the driver, not the server: %s", ae.Msg)
			require.Contains(t, ae.Msg, "10g (DES-based) password verifier")
			require.Contains(t, ae.Msg, "adbc.oracle.allow_10g_verifier=true")
			require.Contains(t, ae.Msg, "man-in-the-middle")
		})
	}
}

func TestLive10gVerifierAllowed(t *testing.T) {
	uri := verifierURI(t, "ADBC_ORACLE_TEST_10G_URI")
	user := uriUser(t, uri)

	t.Run("option", func(t *testing.T) {
		conn, err := openVerifierConn(t, uri, map[string]string{OptionAllow10gVerifier: "true"})
		require.NoError(t, err)
		requireSessionUser(t, conn, user)
	})
	t.Run("uri_parameter", func(t *testing.T) {
		sep := "?"
		if strings.Contains(uri, "?") {
			sep = "&"
		}
		conn, err := openVerifierConn(t, uri+sep+"allow_10g_verifier=true", nil)
		require.NoError(t, err)
		requireSessionUser(t, conn, user)
	})
	t.Run("password_case_insensitive", func(t *testing.T) {
		// The 10g hash upper-cases the password, so the server accepts any case.
		pw := uriPassword(t, uri)
		swapped := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' {
				return r - 'a' + 'A'
			}
			if r >= 'A' && r <= 'Z' {
				return r - 'A' + 'a'
			}
			return r
		}, pw)
		require.NotEqual(t, pw, swapped, "the test password needs letters")
		conn, err := openVerifierConn(t, withPassword(t, uri, swapped), map[string]string{OptionAllow10gVerifier: "true"})
		require.NoError(t, err)
		requireSessionUser(t, conn, user)
	})
}

func TestLive10gVerifierWrongPassword(t *testing.T) {
	uri := verifierURI(t, "ADBC_ORACLE_TEST_10G_URI")
	_, err := openVerifierConn(t, withPassword(t, uri, "definitely-not-the-password"), map[string]string{OptionAllow10gVerifier: "true"})
	ae := requireADBCError(t, err)
	require.EqualValues(t, 1017, ae.VendorCode, ae.Msg)
	require.Equal(t, adbc.StatusUnauthenticated, ae.Code)
	require.Contains(t, ae.Msg, "ORA-01017")
}

func TestLive11gVerifier(t *testing.T) {
	uri := verifierURI(t, "ADBC_ORACLE_TEST_11G_URI")
	user := uriUser(t, uri)
	for _, allow := range []string{"false", "true"} {
		t.Run("allow_10g="+allow, func(t *testing.T) {
			conn, err := openVerifierConn(t, uri, map[string]string{OptionAllow10gVerifier: allow})
			require.NoError(t, err)
			requireSessionUser(t, conn, user)
		})
	}
	t.Run("wrong_password", func(t *testing.T) {
		_, err := openVerifierConn(t, withPassword(t, uri, "definitely-not-the-password"), nil)
		ae := requireADBCError(t, err)
		require.EqualValues(t, 1017, ae.VendorCode, ae.Msg)
	})
}

func TestLive12cVerifierUnaffectedBy10gOption(t *testing.T) {
	uri := verifierURI(t, "ADBC_ORACLE_TEST_12C_URI")
	user := uriUser(t, uri)
	for _, allow := range []string{"", "false", "true"} {
		t.Run("allow_10g="+allow, func(t *testing.T) {
			var extra map[string]string
			if allow != "" {
				extra = map[string]string{OptionAllow10gVerifier: allow}
			}
			conn, err := openVerifierConn(t, uri, extra)
			require.NoError(t, err)
			requireSessionUser(t, conn, user)
		})
	}
	t.Run("wrong_password", func(t *testing.T) {
		_, err := openVerifierConn(t, withPassword(t, uri, "definitely-not-the-password"), map[string]string{OptionAllow10gVerifier: "true"})
		ae := requireADBCError(t, err)
		require.EqualValues(t, 1017, ae.VendorCode, ae.Msg)
	})
}
