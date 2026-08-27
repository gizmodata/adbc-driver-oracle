# Changelog

All notable changes to **adbc-driver-oracle** are documented here. Format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.4.0] - 2026-08-27

### Added

- Native Network Encryption (Oracle Advanced Networking Option): the
  driver negotiates packet encryption and data-integrity checksumming
  (Diffie-Hellman key exchange, AES-128/192/256 + MD5/SHA-1/SHA-256/
  SHA-384/SHA-512) and works with servers that *require* NNE — no Oracle
  Client, no TLS certificates. On by default at `accepted` (transparent
  when the server does not ask for it); `adbc.oracle.nne` =
  `accepted`/`requested`/`required`/`rejected`, with
  `adbc.oracle.nne_checksum` and `adbc.oracle.nne_{encryption,checksum}_algorithms`.

## [0.3.0] - 2026-08-27

### Added

- Object types: user-defined object types, VARRAYs and nested tables are
  returned as JSON text (nested objects, collections, LOB attributes,
  BOOLEAN, dates/intervals); `XMLTYPE` columns as XML text; type
  metadata resolved from the data dictionary and cached per connection.
- `MDSYS.SDO_GEOMETRY` columns are returned as GeoArrow WKB (points,
  lines, polygons incl. rectangles, multi-geometries, collections,
  2D/3D/4D).
- `adbc.oracle.use_extension_types`: Arrow extension-type metadata
  (`arrow.json`, `arrow.opaque` for XMLType, `geoarrow.wkb`).
- Bulk ingest of `list` / `struct` / `map` columns as JSON
  (`adbc.oracle.ingest.struct_type`) and `adbc.oracle.ingest.tablespace`.
- LOB READ support in the wire layer (used for LOB attributes and
  LOB-backed XMLType values).
- CI runs the full `gvenzl/oracle-free:23-faststart` image (with Spatial).

## [0.2.0] - 2026-08-27

### Added

- PL/SQL `OUT` / `IN OUT` bind variables: values are returned as a
  one-row result set with `ORACLE:parameter_type` field metadata; the
  Arrow type follows the bound placeholder's type.
- Implicit result sets (`DBMS_SQL.RETURN_RESULT`) stream back as the
  statement's result set.
- Statement cancellation: `Cancel()` on statements and connections
  (`cursor.adbc_cancel()` in Python) and Go `context` cancellation /
  deadlines, via TCP out-of-band breaks (Linux/macOS) or in-band
  markers (Windows, TLS); the interrupted call fails with `ORA-01013`.
- `adbc.oracle.interval_mode` (`monthdaynano` / `duration` / `string`)
  and `adbc.oracle.date_mode` (`timestamp` / `date32`) type policies.
- `adbc.oracle.batch_bytes`: approximate byte bound per Arrow batch,
  splitting oversized prefetches into zero-copy slices.
- `adbc.oracle.disable_oob` connection option; `float16` binds.

## [0.1.2] - 2026-08-26

### Added
- Integration test and README section for DuckDB's `adbc` community
  extension: `read_adbc`, `ATTACH 'profile://…'`, and INSERT / CTAS into
  Oracle through ADBC bulk ingest, using the driver manifest + a connection
  profile.

### Fixed
- Database, connection and statement now implement the full ADBC
  `GetSetOptions` surface (typed get/set variants). Consumers that query
  connection options — e.g. the DuckDB `adbc` extension's `ATTACH` — failed
  with "AdbcConnectionGetOption: options are not supported".

## [0.1.1] - 2026-08-26

### Fixed
- macOS wheel is built with `MACOSX_DEPLOYMENT_TARGET=12.0` so it installs on
  macOS 12+ instead of only the runner's release (was tagged `macosx_26_0`).

### Changed
- README and the DuckDB/GizmoSQL integration test use the `adbc_scanner`
  `CREATE SECRET` + `ATTACH` pattern (query Oracle as an attached catalog)
  instead of `SET VARIABLE`/`adbc_connect`.
- CI: actions/checkout, setup-go and setup-python bumped to v7.

## [0.1.0] - 2026-08-26

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

[Unreleased]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/gizmodata/adbc-driver-oracle/releases/tag/v0.1.0
