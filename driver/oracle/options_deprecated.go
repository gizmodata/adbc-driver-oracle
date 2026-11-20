package oracle

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// DeprecatedOptions maps each option key used before 1.1.0 (`adbc.oracle.*`)
// to its current name. The old keys still work, for setting and for
// GetOption, at every level that accepted them, but the first use of each one
// in a process prints a single line to stderr naming the old and the new key
// (never the value: options carry passwords and tokens). When both an old and
// a new key are given, the new one wins. The old keys will be removed in 2.0.0.
var DeprecatedOptions = map[string]string{
	"adbc.oracle.host":                      OptionHost,
	"adbc.oracle.port":                      OptionPort,
	"adbc.oracle.service_name":              OptionServiceName,
	"adbc.oracle.sid":                       OptionSID,
	"adbc.oracle.tls":                       OptionTLS,
	"adbc.oracle.tls.ca_cert":               OptionTLSCACert,
	"adbc.oracle.tls.skip_verify":           OptionTLSSkipVerify,
	"adbc.oracle.tls.server_name":           OptionTLSServerName,
	"adbc.oracle.wallet_location":           OptionWalletLocation,
	"adbc.oracle.wallet_password":           OptionWalletPassword,
	"adbc.oracle.token":                     OptionToken,
	"adbc.oracle.mode":                      OptionMode,
	"adbc.oracle.allow_10g_verifier":        OptionAllow10gVerifier,
	"adbc.oracle.connect_timeout":           OptionConnectTimeoutSeconds,
	"adbc.oracle.batch_size":                OptionBatchSize,
	"adbc.oracle.batch_bytes":               OptionBatchBytes,
	"adbc.oracle.prefetch_rows":             OptionPrefetchRows,
	"adbc.oracle.number_mode":               OptionNumberType,
	"adbc.oracle.interval_mode":             OptionIntervalType,
	"adbc.oracle.date_mode":                 OptionDateType,
	"adbc.oracle.use_extension_types":       OptionUseExtensionTypes,
	"adbc.oracle.session_time_zone":         OptionSessionTimeZone,
	"adbc.oracle.application_name":          OptionApplicationName,
	"adbc.oracle.current_schema":            OptionCurrentSchema,
	"adbc.oracle.module":                    OptionSessionModule,
	"adbc.oracle.action":                    OptionSessionAction,
	"adbc.oracle.client_info":               OptionSessionClientInfo,
	"adbc.oracle.client_identifier":         OptionSessionClientIdentifier,
	"adbc.oracle.nne":                       OptionNNE,
	"adbc.oracle.nne_checksum":              OptionNNEChecksum,
	"adbc.oracle.nne_encryption_algorithms": OptionNNEEncryptionAlgorithms,
	"adbc.oracle.nne_checksum_algorithms":   OptionNNEChecksumAlgorithms,
	"adbc.oracle.nne_active":                OptionNNEActive,
	"adbc.oracle.nne_algorithms":            OptionNNEAlgorithms,
	"adbc.oracle.disable_oob":               OptionDisableOOB,
	"adbc.oracle.sdu":                       OptionSDU,
	"adbc.oracle.trace":                     OptionTrace,
	"adbc.oracle.ingest.batch_rows":         OptionIngestBatchRows,
	"adbc.oracle.ingest.varchar_length":     OptionIngestVarcharLength,
	"adbc.oracle.ingest.raw_length":         OptionIngestRawLength,
	"adbc.oracle.ingest.string_type":        OptionIngestStringType,
	"adbc.oracle.ingest.binary_type":        OptionIngestBinaryType,
	"adbc.oracle.ingest.struct_type":        OptionIngestStructType,
	"adbc.oracle.ingest.tablespace":         OptionIngestTablespace,
}

// oldOptionFor is the reverse of DeprecatedOptions: new key -> old key.
var oldOptionFor = func() map[string]string {
	m := make(map[string]string, len(DeprecatedOptions))
	for oldKey, newKey := range DeprecatedOptions {
		m[newKey] = oldKey
	}
	return m
}()

var (
	// deprecationWarned records the old keys already warned about in this
	// process.
	deprecationWarned sync.Map
	// deprecationMu guards deprecationOut.
	deprecationMu  sync.Mutex
	deprecationOut io.Writer = os.Stderr
)

// setDeprecationWriter redirects the deprecation warnings (tests) and returns
// the previous writer.
func setDeprecationWriter(w io.Writer) io.Writer {
	deprecationMu.Lock()
	defer deprecationMu.Unlock()
	prev := deprecationOut
	deprecationOut = w
	return prev
}

// canonicalOption returns the current name of an option key. For an old key
// it warns once per process (key names only, never a value).
func canonicalOption(key string) string {
	newKey, ok := DeprecatedOptions[key]
	if !ok {
		return key
	}
	if _, warned := deprecationWarned.LoadOrStore(key, struct{}{}); !warned {
		deprecationMu.Lock()
		fmt.Fprintf(deprecationOut, "[oracle] option %q is deprecated; use %q\n", key, newKey)
		deprecationMu.Unlock()
	}
	return newKey
}

// canonicalOptions returns opts with every old key renamed to its current
// name. A new key given alongside its old one wins, whatever the order.
func canonicalOptions(opts map[string]string) map[string]string {
	out := make(map[string]string, len(opts))
	for k, v := range opts {
		if _, isOld := DeprecatedOptions[k]; !isOld {
			out[k] = v
		}
	}
	for k, v := range opts {
		if _, isOld := DeprecatedOptions[k]; !isOld {
			continue
		}
		newKey := canonicalOption(k)
		if _, haveNew := opts[newKey]; !haveNew {
			out[newKey] = v
		}
	}
	return out
}
