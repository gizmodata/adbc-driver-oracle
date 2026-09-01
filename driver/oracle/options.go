package oracle

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-adbc/go/adbc"

	"github.com/gizmodata/adbc-driver-oracle/internal/tns"
	"github.com/gizmodata/adbc-driver-oracle/internal/ttc"
)

// ADBC option keys. They follow the `adbc.<vendor>.<noun>` convention.
const (
	// OptionURI is "uri": oracle://[user[:password]@]host[:port]/SERVICE[?params],
	// an Easy Connect string (host[:port]/service) or a full
	// (DESCRIPTION=...) connect descriptor.
	OptionURI      = adbc.OptionKeyURI
	OptionUsername = adbc.OptionKeyUsername
	OptionPassword = adbc.OptionKeyPassword

	OptionHost        = "adbc.oracle.host"
	OptionPort        = "adbc.oracle.port"
	OptionServiceName = "adbc.oracle.service_name"
	OptionSID         = "adbc.oracle.sid"
	// OptionTLS enables TLS ("tcps") transport.
	OptionTLS = "adbc.oracle.tls"
	// OptionTLSCACert is a path to a PEM CA bundle used to verify the server.
	OptionTLSCACert = "adbc.oracle.tls.ca_cert"
	// OptionTLSSkipVerify disables server certificate verification.
	OptionTLSSkipVerify = "adbc.oracle.tls.skip_verify"
	// OptionTLSServerName overrides the SNI/verification host name.
	OptionTLSServerName = "adbc.oracle.tls.server_name"
	// OptionWalletLocation is a directory containing ewallet.pem (Autonomous
	// Database wallets); implies TLS.
	OptionWalletLocation = "adbc.oracle.wallet_location"
	// OptionWalletPassword is the ewallet.pem private key password.
	OptionWalletPassword = "adbc.oracle.wallet_password"
	// OptionConnectTimeout: plain digits are seconds, else a Go duration.
	OptionConnectTimeout = "adbc.oracle.connect_timeout"
	// OptionBatchSize caps rows per Arrow record batch (default 65536).
	OptionBatchSize = "adbc.oracle.batch_size"
	// OptionPrefetchRows is the number of rows fetched per server round
	// trip (default: batch size).
	OptionPrefetchRows = "adbc.oracle.prefetch_rows"
	// OptionTrace: "true" logs TNS packets to stderr.
	OptionTrace = "adbc.oracle.trace"
	// OptionApplicationName is reported as the client program name.
	OptionApplicationName = "adbc.oracle.application_name"
	// OptionCurrentSchema sets the session's current schema after connect.
	OptionCurrentSchema = "adbc.oracle.current_schema"
	// OptionMode selects a privileged connection: "sysdba", "sysoper", ...
	OptionMode = "adbc.oracle.mode"
	// OptionSessionTimeZone sets the session TIME_ZONE (default "+00:00" so
	// TIMESTAMP WITH LOCAL TIME ZONE values arrive as UTC).
	OptionSessionTimeZone = "adbc.oracle.session_time_zone"
	// OptionIntervalMode controls how INTERVAL columns map to Arrow:
	// "monthdaynano" (default), "duration" (DAY TO SECOND only; YEAR TO
	// MONTH stays month_day_nano_interval) or "string" (ISO-8601 text).
	OptionIntervalMode = "adbc.oracle.interval_mode"
	// OptionDateMode controls how DATE columns map to Arrow: "timestamp"
	// (default, timestamp[s] — Oracle DATE has a time component) or
	// "date32" (drops the time of day).
	OptionDateMode = "adbc.oracle.date_mode"
	// OptionBatchBytes caps the approximate size of an Arrow record batch
	// in bytes (default 8 MiB — keeps a batch under common transport
	// limits such as gRPC's 16 MiB Flight SQL message cap; 0 = unlimited;
	// rows are still capped by batch_size).
	OptionBatchBytes = "adbc.oracle.batch_bytes"
	// OptionDisableOOB disables out-of-band (TCP urgent data) breaks used
	// for statement cancellation.
	OptionDisableOOB = "adbc.oracle.disable_oob"
	// OptionNNE controls Native Network Encryption / data integrity
	// (Oracle Advanced Networking): "accepted" (default — negotiate if the
	// server asks), "requested", "required" (fail if the server won't
	// encrypt), or "rejected"/"off" (never negotiate).
	OptionNNE = "adbc.oracle.nne"
	// OptionNNEChecksum controls the data-integrity level independently of
	// encryption (same values as OptionNNE); defaults to follow OptionNNE.
	OptionNNEChecksum = "adbc.oracle.nne_checksum"
	// OptionNNEEncryptionAlgorithms restricts the offered encryption
	// algorithms (comma-separated, e.g. "AES256,AES192").
	OptionNNEEncryptionAlgorithms = "adbc.oracle.nne_encryption_algorithms"
	// OptionNNEChecksumAlgorithms restricts the offered checksum algorithms
	// (comma-separated, e.g. "SHA256,SHA512").
	OptionNNEChecksumAlgorithms = "adbc.oracle.nne_checksum_algorithms"
	// OptionNNEActive (read-only, connection) reports whether Native
	// Network Encryption / data integrity is active on the session:
	// "true" or "false".
	OptionNNEActive = "adbc.oracle.nne_active"
	// OptionNNEAlgorithms (read-only, connection) reports the negotiated
	// algorithms as "<encryption>,<checksum>" (e.g. "AES256,SHA512";
	// empty entries when a service is off, "" when NNE is inactive).
	OptionNNEAlgorithms = "adbc.oracle.nne_algorithms"
	// OptionUseExtensionTypes annotates JSON, object (arrow.json),
	// SDO_GEOMETRY (geoarrow.wkb) and XMLType (arrow.opaque) columns with
	// Arrow extension-type metadata.
	OptionUseExtensionTypes = "adbc.oracle.use_extension_types"
	// OptionNumberMode controls how NUMBER columns map to Arrow:
	// "auto" (default: int64 / decimal128 / float64 by precision and
	// scale), "decimal" (decimal128 wherever possible), "double", "string".
	OptionNumberMode = "adbc.oracle.number_mode"
	// OptionSDU requests a session data unit size (bytes).
	OptionSDU = "adbc.oracle.sdu"
	// OptionToken authenticates with an OAuth / IAM bearer token (TLS only).
	OptionToken = "adbc.oracle.token"

	// Statement options for bulk ingest.
	OptionIngestBatchRows     = "adbc.oracle.ingest.batch_rows"
	OptionIngestVarcharLength = "adbc.oracle.ingest.varchar_length"
	OptionIngestRawLength     = "adbc.oracle.ingest.raw_length"
	OptionIngestStringType    = "adbc.oracle.ingest.string_type" // VARCHAR2 (default) or CLOB
	OptionIngestBinaryType    = "adbc.oracle.ingest.binary_type" // RAW (default) or BLOB
	OptionIngestStructType    = "adbc.oracle.ingest.struct_type" // JSON (default on 21c+), CLOB, VARCHAR2, BLOB
	OptionIngestTablespace    = "adbc.oracle.ingest.tablespace"

	OptionIngestTable = adbc.OptionKeyIngestTargetTable
)

// Number mapping modes.
const (
	NumberModeAuto    = "auto"
	NumberModeDecimal = "decimal"
	NumberModeDouble  = "double"
	NumberModeString  = "string"
)

// Interval mapping modes.
const (
	IntervalModeMonthDayNano = "monthdaynano"
	IntervalModeDuration     = "duration"
	IntervalModeString       = "string"
)

// Date mapping modes.
const (
	DateModeTimestamp = "timestamp"
	DateModeDate32    = "date32"
)

// typeOptions collects the Arrow mapping policies.
type typeOptions struct {
	numberMode        string
	intervalMode      string
	dateMode          string
	useExtensionTypes bool
}

func parseIntervalMode(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case IntervalModeMonthDayNano, "", "auto", "month_day_nano":
		return IntervalModeMonthDayNano, nil
	case IntervalModeDuration:
		return IntervalModeDuration, nil
	case IntervalModeString, "text":
		return IntervalModeString, nil
	}
	return "", errStatus(adbc.StatusInvalidArgument, "oracle: unknown %s %q", OptionIntervalMode, v)
}

func parseDateMode(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case DateModeTimestamp, "", "auto":
		return DateModeTimestamp, nil
	case DateModeDate32, "date":
		return DateModeDate32, nil
	}
	return "", errStatus(adbc.StatusInvalidArgument, "oracle: unknown %s %q", OptionDateMode, v)
}

// connConfig is the resolved connection configuration.
type connConfig struct {
	ttc          ttc.Config
	batchSize    int
	batchBytes   int64
	prefetchRows int
	trace        bool
	types        typeOptions
	nneEnc       int
	nneChk       int
	nneEncSet    bool
	nneChkSet    bool
	nneEncAlgos  []string
	nneChkAlgos  []string
}

var easyConnectRe = regexp.MustCompile(`^(?:(tcps?)://)?([^:/]+)(?::(\d+))?/([^?:]+)(?::([A-Za-z]+))?(?:\?(.*))?$`)

// parseOptions merges the URI and explicit ADBC options into a config.
// Explicit options override URI components.
func parseOptions(opts map[string]string) (*connConfig, error) {
	// batchBytes defaults to 8 MiB so a single batch stays under common
	// transport limits (e.g. Flight SQL's 16 MiB gRPC message cap) even
	// for wide rows; set batch_bytes=0 for unlimited.
	cfg := &connConfig{batchSize: 65536, batchBytes: 8 << 20, types: typeOptions{numberMode: NumberModeAuto, intervalMode: IntervalModeMonthDayNano, dateMode: DateModeTimestamp}}
	cfg.ttc.ConnectTimeout = 30 * time.Second
	cfg.ttc.FullVersion = fullVersionNum()
	var tlsEnabled, skipVerify bool
	var caCert, serverName, walletLocation, walletPassword string
	var host string
	port := 0

	setParam := func(key, v string) error {
		switch strings.ToLower(key) {
		case "tls", "ssl", "tcps":
			tlsEnabled = isTrue(v)
		case "tls_ca_cert", "ca_cert", "ssl_ca":
			caCert = v
		case "tls_skip_verify", "ssl_skip_verify":
			skipVerify = isTrue(v)
		case "tls_server_name", "ssl_server_name":
			serverName = v
		case "wallet_location", "wallet":
			walletLocation = v
		case "wallet_password":
			walletPassword = v
		case "service_name", "service":
			cfg.ttc.ServiceName = v
		case "sid":
			cfg.ttc.SID = v
		case "instance_name":
			cfg.ttc.InstanceName = v
		case "server", "server_type":
			cfg.ttc.ServerType = strings.ToLower(v)
		case "schema", "current_schema", "currentschema":
			cfg.ttc.CurrentSchema = v
		case "connect_timeout", "logintimeout", "tcp_connect_timeout":
			d, err := parseTimeout(v)
			if err != nil {
				return err
			}
			cfg.ttc.ConnectTimeout = d
		case "application_name", "program", "applicationname":
			cfg.ttc.Program = v
		case "batch_size":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return errStatus(adbc.StatusInvalidArgument, "oracle: invalid batch_size %q", v)
			}
			cfg.batchSize = n
		case "prefetch_rows", "arraysize":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return errStatus(adbc.StatusInvalidArgument, "oracle: invalid prefetch_rows %q", v)
			}
			cfg.prefetchRows = n
		case "user", "username", "uid":
			cfg.ttc.Username = v
		case "password", "pwd":
			cfg.ttc.Password = v
		case "mode":
			m, err := parseMode(v)
			if err != nil {
				return err
			}
			cfg.ttc.Mode = m
		case "session_time_zone", "time_zone":
			cfg.ttc.SessionTimeZone = v
		case "number_mode":
			nm, err := parseNumberMode(v)
			if err != nil {
				return err
			}
			cfg.types.numberMode = nm
		case "interval_mode":
			im, err := parseIntervalMode(v)
			if err != nil {
				return err
			}
			cfg.types.intervalMode = im
		case "date_mode":
			dm, err := parseDateMode(v)
			if err != nil {
				return err
			}
			cfg.types.dateMode = dm
		case "batch_bytes":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return errStatus(adbc.StatusInvalidArgument, "oracle: invalid batch_bytes %q", v)
			}
			cfg.batchBytes = n
		case "disable_oob":
			cfg.ttc.DisableOOB = isTrue(v)
		case "use_extension_types":
			cfg.types.useExtensionTypes = isTrue(v)
		case "nne", "encryption":
			l, err := parseNNELevel(v)
			if err != nil {
				return err
			}
			cfg.nneEnc = l
			cfg.nneEncSet = true
		case "nne_checksum", "data_integrity":
			l, err := parseNNELevel(v)
			if err != nil {
				return err
			}
			cfg.nneChk = l
			cfg.nneChkSet = true
		case "nne_encryption_algorithms":
			cfg.nneEncAlgos = splitCSV(v)
		case "nne_checksum_algorithms":
			cfg.nneChkAlgos = splitCSV(v)
		case "sdu":
			n, err := strconv.Atoi(v)
			if err != nil || n < 512 {
				return errStatus(adbc.StatusInvalidArgument, "oracle: invalid sdu %q", v)
			}
			cfg.ttc.SDU = uint32(n)
		case "token":
			cfg.ttc.Token = v
		case "trace":
			cfg.trace = isTrue(v)
		default:
			return errStatus(adbc.StatusInvalidArgument, "oracle: unknown URI parameter %q", key)
		}
		return nil
	}

	if raw := strings.TrimSpace(opts[OptionURI]); raw != "" {
		switch {
		case strings.HasPrefix(raw, "("):
			// Full connect descriptor.
			if err := parseDescriptor(raw, cfg, &tlsEnabled); err != nil {
				return nil, err
			}
		case strings.Contains(raw, "://") && !strings.HasPrefix(strings.ToLower(raw), "tcp://") && !strings.HasPrefix(strings.ToLower(raw), "tcps://"):
			u, err := url.Parse(raw)
			if err != nil {
				return nil, errStatus(adbc.StatusInvalidArgument, "oracle: invalid URI %q: %v", raw, err)
			}
			switch strings.ToLower(u.Scheme) {
			case "oracle", "oracledb", "ora":
			case "oracles", "oracle+tcps":
				tlsEnabled = true
			default:
				return nil, errStatus(adbc.StatusInvalidArgument, "oracle: unsupported URI scheme %q (want oracle://)", u.Scheme)
			}
			h, p, err := net.SplitHostPort(u.Host)
			if err != nil {
				h = u.Host
				p = ""
			}
			host = h
			if p != "" {
				port, err = strconv.Atoi(p)
				if err != nil {
					return nil, errStatus(adbc.StatusInvalidArgument, "oracle: invalid port %q", p)
				}
			}
			if u.User != nil {
				cfg.ttc.Username = u.User.Username()
				if pw, ok := u.User.Password(); ok {
					cfg.ttc.Password = pw
				}
			}
			path := strings.Trim(u.Path, "/")
			if path != "" {
				cfg.ttc.ServiceName = path
			}
			for key, vals := range u.Query() {
				if len(vals) == 0 {
					continue
				}
				if err := setParam(key, vals[0]); err != nil {
					return nil, err
				}
			}
		default:
			// Easy Connect: [tcp[s]://]host[:port]/service[:server][?params]
			m := easyConnectRe.FindStringSubmatch(raw)
			if m == nil {
				return nil, errStatus(adbc.StatusInvalidArgument, "oracle: cannot parse connect string %q", raw)
			}
			if strings.EqualFold(m[1], "tcps") {
				tlsEnabled = true
			}
			host = m[2]
			if m[3] != "" {
				port, _ = strconv.Atoi(m[3])
			}
			cfg.ttc.ServiceName = m[4]
			if m[5] != "" {
				cfg.ttc.ServerType = strings.ToLower(m[5])
			}
			if m[6] != "" {
				q, err := url.ParseQuery(m[6])
				if err != nil {
					return nil, errStatus(adbc.StatusInvalidArgument, "oracle: invalid parameters in %q", raw)
				}
				for key, vals := range q {
					if err := setParam(key, vals[0]); err != nil {
						return nil, err
					}
				}
			}
		}
	}

	for k, v := range opts {
		switch k {
		case OptionURI:
		case OptionUsername:
			cfg.ttc.Username = v
		case OptionPassword:
			cfg.ttc.Password = v
		case OptionHost:
			host = v
		case OptionPort:
			p, err := strconv.Atoi(v)
			if err != nil {
				return nil, errStatus(adbc.StatusInvalidArgument, "oracle: invalid %s %q", k, v)
			}
			port = p
		case OptionServiceName:
			cfg.ttc.ServiceName = v
		case OptionSID:
			cfg.ttc.SID = v
		case OptionTLS:
			tlsEnabled = isTrue(v)
		case OptionTLSCACert:
			caCert = v
		case OptionTLSSkipVerify:
			skipVerify = isTrue(v)
		case OptionTLSServerName:
			serverName = v
		case OptionWalletLocation:
			walletLocation = v
		case OptionWalletPassword:
			walletPassword = v
		case OptionCurrentSchema:
			cfg.ttc.CurrentSchema = v
		case OptionConnectTimeout:
			d, err := parseTimeout(v)
			if err != nil {
				return nil, err
			}
			cfg.ttc.ConnectTimeout = d
		case OptionApplicationName:
			cfg.ttc.Program = v
		case OptionBatchSize:
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, errStatus(adbc.StatusInvalidArgument, "oracle: invalid %s %q", k, v)
			}
			cfg.batchSize = n
		case OptionPrefetchRows:
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, errStatus(adbc.StatusInvalidArgument, "oracle: invalid %s %q", k, v)
			}
			cfg.prefetchRows = n
		case OptionTrace:
			cfg.trace = isTrue(v)
		case OptionMode:
			m, err := parseMode(v)
			if err != nil {
				return nil, err
			}
			cfg.ttc.Mode = m
		case OptionSessionTimeZone:
			cfg.ttc.SessionTimeZone = v
		case OptionNumberMode:
			nm, err := parseNumberMode(v)
			if err != nil {
				return nil, err
			}
			cfg.types.numberMode = nm
		case OptionIntervalMode:
			im, err := parseIntervalMode(v)
			if err != nil {
				return nil, err
			}
			cfg.types.intervalMode = im
		case OptionDateMode:
			dm, err := parseDateMode(v)
			if err != nil {
				return nil, err
			}
			cfg.types.dateMode = dm
		case OptionBatchBytes:
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return nil, errStatus(adbc.StatusInvalidArgument, "oracle: invalid %s %q", k, v)
			}
			cfg.batchBytes = n
		case OptionDisableOOB:
			cfg.ttc.DisableOOB = isTrue(v)
		case OptionUseExtensionTypes:
			cfg.types.useExtensionTypes = isTrue(v)
		case OptionNNE:
			l, err := parseNNELevel(v)
			if err != nil {
				return nil, err
			}
			cfg.nneEnc = l
			cfg.nneEncSet = true
		case OptionNNEChecksum:
			l, err := parseNNELevel(v)
			if err != nil {
				return nil, err
			}
			cfg.nneChk = l
			cfg.nneChkSet = true
		case OptionNNEEncryptionAlgorithms:
			cfg.nneEncAlgos = splitCSV(v)
		case OptionNNEChecksumAlgorithms:
			cfg.nneChkAlgos = splitCSV(v)
		case OptionSDU:
			n, err := strconv.Atoi(v)
			if err != nil || n < 512 {
				return nil, errStatus(adbc.StatusInvalidArgument, "oracle: invalid %s %q", k, v)
			}
			cfg.ttc.SDU = uint32(n)
		case OptionToken:
			cfg.ttc.Token = v
		default:
			// Unknown options are ignored so generic tooling that sets
			// e.g. adbc.connection.autocommit at the database level
			// doesn't fail.
		}
	}

	if host != "" {
		cfg.ttc.Addresses = []ttc.Address{{Host: host, Port: port}}
	}
	if len(cfg.ttc.Addresses) == 0 {
		return nil, errStatus(adbc.StatusInvalidArgument, "oracle: no host given (set %s or %s)", OptionURI, OptionHost)
	}
	if cfg.ttc.ServiceName == "" && cfg.ttc.SID == "" {
		return nil, errStatus(adbc.StatusInvalidArgument, "oracle: no service name given (URI path, e.g. oracle://host:1521/FREEPDB1, or %s)", OptionServiceName)
	}
	if cfg.ttc.Username == "" && cfg.ttc.Token == "" {
		return nil, errStatus(adbc.StatusInvalidArgument, "oracle: no username given (set %s)", OptionUsername)
	}
	if cfg.prefetchRows == 0 {
		cfg.prefetchRows = cfg.batchSize
		if cfg.prefetchRows > 65536 {
			cfg.prefetchRows = 65536
		}
	}
	if walletLocation != "" {
		tlsEnabled = true
	}
	if tlsEnabled {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if serverName != "" {
			tc.ServerName = serverName
		}
		if skipVerify {
			tc.InsecureSkipVerify = true //nolint:gosec // explicit user opt-in
		}
		if caCert != "" {
			pem, err := os.ReadFile(caCert)
			if err != nil {
				return nil, errStatus(adbc.StatusInvalidArgument, "oracle: read CA cert %q: %v", caCert, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errStatus(adbc.StatusInvalidArgument, "oracle: no certificates found in %q", caCert)
			}
			tc.RootCAs = pool
		}
		if walletLocation != "" {
			if err := loadWallet(tc, walletLocation, walletPassword); err != nil {
				return nil, err
			}
		}
		cfg.ttc.TLS = tc
		for i := range cfg.ttc.Addresses {
			cfg.ttc.Addresses[i].Protocol = "tcps"
		}
	}
	// Native Network Encryption: enabled by default at "accepted" over plain
	// TCP (over TLS the channel is already encrypted, so it stays off unless
	// explicitly requested).
	encLevel := cfg.nneEnc
	if !cfg.nneEncSet {
		if cfg.ttc.TLS == nil {
			encLevel = tns.LevelAccepted
		} else {
			encLevel = tns.LevelRejected
		}
	}
	chkLevel := cfg.nneChk
	if !cfg.nneChkSet {
		chkLevel = encLevel
	}
	if encLevel != tns.LevelRejected || chkLevel != tns.LevelRejected {
		cfg.ttc.ANO = &tns.ANOConfig{
			EncryptionLevel: encLevel,
			ChecksumLevel:   chkLevel,
			Encryption:      cfg.nneEncAlgos,
			Checksum:        cfg.nneChkAlgos,
		}
	}
	return cfg, nil
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseDescriptor extracts addresses and connect data from a
// (DESCRIPTION=...) string.
func parseDescriptor(desc string, cfg *connConfig, tlsEnabled *bool) error {
	upper := strings.ToUpper(desc)
	get := func(key string, from int) (string, int) {
		idx := strings.Index(upper[from:], "("+key+"=")
		if idx < 0 {
			return "", -1
		}
		start := from + idx + len(key) + 2
		depth := 1
		i := start
		for i < len(desc) && depth > 0 {
			switch desc[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth > 0 {
				i++
			}
		}
		return strings.TrimSpace(desc[start:i]), i
	}
	for pos := 0; ; {
		addr, end := get("ADDRESS", pos)
		if end < 0 {
			break
		}
		pos = end
		au := strings.ToUpper(addr)
		if !strings.Contains(au, "(HOST=") {
			continue
		}
		var a ttc.Address
		sub := func(key string) string {
			i := strings.Index(au, "("+key+"=")
			if i < 0 {
				return ""
			}
			rest := addr[i+len(key)+2:]
			j := strings.IndexByte(rest, ')')
			if j < 0 {
				return ""
			}
			return strings.TrimSpace(rest[:j])
		}
		a.Host = sub("HOST")
		a.Port, _ = strconv.Atoi(sub("PORT"))
		a.Protocol = strings.ToLower(sub("PROTOCOL"))
		if a.Protocol == "tcps" {
			*tlsEnabled = true
		}
		cfg.ttc.Addresses = append(cfg.ttc.Addresses, a)
	}
	if v, _ := get("SERVICE_NAME", 0); v != "" {
		cfg.ttc.ServiceName = v
	}
	if v, _ := get("SID", 0); v != "" {
		cfg.ttc.SID = v
	}
	if v, _ := get("INSTANCE_NAME", 0); v != "" {
		cfg.ttc.InstanceName = v
	}
	if v, _ := get("SERVER", 0); v != "" {
		cfg.ttc.ServerType = strings.ToLower(v)
	}
	if v, _ := get("MY_WALLET_DIRECTORY", 0); v != "" {
		cfg.ttc.ExtraConnectData = nil
	}
	if len(cfg.ttc.Addresses) == 0 {
		return errStatus(adbc.StatusInvalidArgument, "oracle: connect descriptor has no (ADDRESS=(HOST=...)) entry")
	}
	return nil
}

// loadWallet loads ewallet.pem (Autonomous Database wallet) into the TLS
// config: CA certificates for verification and, if present, a client
// certificate/key for mTLS.
func loadWallet(tc *tls.Config, dir, password string) error {
	path := dir
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		path = dir + string(os.PathSeparator) + "ewallet.pem"
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return errStatus(adbc.StatusInvalidArgument, "oracle: cannot read wallet %q: %v", path, err)
	}
	pool := x509.NewCertPool()
	if pool.AppendCertsFromPEM(pem) {
		tc.RootCAs = pool
	}
	if cert, err := tls.X509KeyPair(pem, pem); err == nil {
		tc.Certificates = []tls.Certificate{cert}
	} else if password != "" {
		// Encrypted private keys are not supported by crypto/tls directly.
		return errStatus(adbc.StatusNotImplemented, "oracle: password-protected wallet private keys are not supported; convert ewallet.pem to an unencrypted key")
	}
	return nil
}

func parseMode(v string) (uint32, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "default", "normal":
		return 0, nil
	case "sysdba":
		return 0x20, nil
	case "sysoper":
		return 0x40, nil
	case "sysasm":
		return 0x400000, nil
	case "sysbkp", "sysbackup":
		return 0x1000000, nil
	case "sysdgd", "sysdg":
		return 0x2000000, nil
	case "syskmt", "syskm":
		return 0x4000000, nil
	case "sysrac":
		return 0x8000000, nil
	}
	return 0, errStatus(adbc.StatusInvalidArgument, "oracle: unknown mode %q", v)
}

func parseNNELevel(v string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "accepted":
		return tns.LevelAccepted, nil
	case "requested":
		return tns.LevelRequested, nil
	case "required":
		return tns.LevelRequired, nil
	case "rejected", "off", "disabled", "none":
		return tns.LevelRejected, nil
	}
	return 0, errStatus(adbc.StatusInvalidArgument, "oracle: unknown NNE level %q (accepted|requested|required|rejected)", v)
}

func parseNumberMode(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case NumberModeAuto, "":
		return NumberModeAuto, nil
	case NumberModeDecimal:
		return NumberModeDecimal, nil
	case NumberModeDouble, "float", "float64":
		return NumberModeDouble, nil
	case NumberModeString, "text":
		return NumberModeString, nil
	}
	return "", errStatus(adbc.StatusInvalidArgument, "oracle: unknown %s %q", OptionNumberMode, v)
}

func fullVersionNum() uint32 {
	parts := strings.SplitN(Version, ".", 3)
	var nums [3]uint32
	for i := 0; i < len(parts) && i < 3; i++ {
		n, _ := strconv.Atoi(strings.TrimLeft(parts[i], "v"))
		nums[i] = uint32(n)
	}
	return nums[0]<<24 | nums[1]<<20 | nums[2]<<12
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func parseTimeout(v string) (time.Duration, error) {
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, errStatus(adbc.StatusInvalidArgument, "oracle: invalid timeout %q: %v", v, err)
	}
	return d, nil
}

func errStatus(code adbc.Status, format string, args ...any) error {
	return adbc.Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}
