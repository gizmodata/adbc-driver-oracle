# Changelog

All notable changes to **adbc-driver-oracle** are documented here. Format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Initial pure-Go ADBC driver for Oracle Database: TNS/TTC wire protocol
  implementation (no Oracle Client libraries) with TCP and TLS (`tcps`)
  transports, listener redirects, O5LOGON (11g/12c verifiers) and 23ai
  fast-auth, Arrow record-batch streaming reads, `?` / `:name` parameter
  binding with array DML, transactions, `GetInfo` / `GetObjects` (tables,
  views, columns, primary/unique/foreign keys) / `GetTableSchema` /
  `GetTableTypes` metadata, array-bind bulk ingest (`create` / `append`
  / `replace` / `create_append`, temporary tables), cgo c-shared library,
  Python wheel (`pip install adbc-driver-oracle`) with DBAPI wrapper, and
  `oracle.toml` driver-manifest installer for connection profiles /
  `oracle://` URI-scheme resolution.
- Type coverage: NUMBER (int64 / decimal128 / float64 by precision, or
  `adbc.oracle.number_mode`), BINARY_FLOAT/DOUBLE, CHAR/VARCHAR2/
  NCHAR/NVARCHAR2/LONG, CLOB/NCLOB/BLOB (fetched inline), RAW/LONG RAW,
  DATE, TIMESTAMP (unit by precision), TIMESTAMP WITH (LOCAL) TIME ZONE,
  INTERVAL DAY TO SECOND / YEAR TO MONTH, ROWID/UROWID, BOOLEAN (23ai)
  and JSON (native OSON decoded to text client-side).
- Connection options: host/port/service_name/sid, TLS (CA bundle, skip
  verify, server name), Autonomous Database wallets (`ewallet.pem`),
  token authentication, privileged modes (`sysdba` …), connect timeout,
  batch size, prefetch rows, session time zone, SDU, application name,
  current schema, packet tracing; end-to-end tracing attributes
  (module / action / client info / identifier) per connection.
- Cross-tool integration tests: Oracle → GizmoSQL streaming ingest
  (ADBC to ADBC) and DuckDB `adbc_scanner` pull/push against Oracle
  through the c-shared driver (GizmoSQL / DuckDB are test-only
  dependencies).
