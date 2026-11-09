# Changelog

All notable changes to **adbc-driver-oracle** are documented here. Format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.3] - 2026-11-09

The driver now passes the [adbc-drivers.org driver validation suite](https://github.com/adbc-drivers/validation)
(the ADBC conformance tests) against Oracle 23ai. Running it found the fixes below.

### Fixed

- **`TIMESTAMP` and `DATE` values before 1677-09-21 or after 2262-04-11 were read back as wrong
  dates** when the column maps to `timestamp[ms]` or `timestamp[us]` (`TIMESTAMP(1)` to
  `TIMESTAMP(6)`), and as text inside JSON and object-type values: `0001-01-01` came back as
  `1754-08-30`, `9999-12-31` as `1816-03-30`. The conversion went through int64 nanoseconds,
  which overflow outside that range. Every Oracle date (years 1 to 9999) now reads back exactly; a
  `TIMESTAMP(7..9)` value that `timestamp[ns]` can't hold fails with `INVALID_DATA` instead of
  wrapping. `date_mode=date32` now gives a pre-1970 value with a time of day its own date.
- **A bound parameter stream ran only its first batch.** `executemany` over several batches, or
  `bind_stream` with DML, silently skipped every later batch; an empty batch (or an empty bound
  record) panicked the driver (`index out of range`), leaving it unusable until the process
  restarted. Every batch now runs, empty ones are skipped, and the row count is the total.
- A query bound to several parameter rows failed (`cannot execute a query with multiple bind
  rows`); it now runs once per row and returns the results in order, as ADBC specifies.
- Dictionary-encoded columns (e.g. pandas categoricals) can be bound and ingested (as their
  value type); they were refused.
- Appending to an existing table whose columns don't match the data now fails with ADBC's
  `ALREADY_EXISTS` (was `INVALID_ARGUMENT`).
- `GetObjects` reports `CHECK` constraints (it listed only primary, unique and foreign keys).
  Oracle's own `NOT NULL` constraints stay out: nullability is reported per column.
- `GetTableSchema` finds a table by its exact name first, so a table created with a quoted
  lower- or mixed-case name (as `GetObjects` reports it) is found; unquoted-style names still
  resolve upper-cased as before.
- `GetInfo`'s driver Arrow version is the arrow-go release the driver was built with (e.g.
  `v18.8.0`), not the fixed text `arrow-go/v18`.
- **The macOS wheel claimed more than it supports.** It was tagged `macosx_12_0_universal2`, but
  the bundled library is arm64-only and is built with Go 1.27, whose runtime needs macOS 13. pip
  could install 1.0.2 on an Intel Mac or on macOS 12, where the driver can't load. The wheel is now
  `macosx_13_0_arm64` (Intel Macs get no wheel, which pip reports as no matching distribution), and
  CI fails if the wheel's tag doesn't match the library's minimum macOS and architecture.

### Changed

- An unknown database option is refused with `NOT_IMPLEMENTED`, as ADBC specifies, so a misspelt
  key fails instead of being ignored. Standard ADBC connection, statement and ingest keys
  (`adbc.connection.*`, `adbc.statement.*`, `adbc.ingest.*`) set at the database level are still
  accepted.
- README type mapping: `NUMBER` with a negative scale maps to `float64` (it always did; the table
  said `decimal128`).
- CI runs the validation suite (`validation/`, a pinned test-only dependency) against an Oracle
  service container, and wheels, PyPI and the GitHub Release now also wait for it. It fails,
  never skips, when the server is unreachable or a setting is missing.
- `make -C pkg/oracle` always runs `go build` (Go's cache skips unchanged work); it used to keep a
  stale library after changes outside `pkg/oracle`.
- Dependencies: golang.org/x/{exp,mod,sync,sys,tools}, goccy/go-json 0.12, klauspost/compress
  1.20.4, klauspost/cpuid 2.4.2, pierrec/lz4 4.1.33, go.yaml.in/yaml 3.0.6 (arrow-adbc 1.13.0 and
  arrow-go 18.8.0 are the latest releases already). Test extras: pytest 9.2, pandas 3.1.1,
  hardcharger 2.10. Go 1.27.3 in CI and the GitHub Actions versions were already current. The
  runtime floors (`adbc-driver-manager`, `pyarrow`) are unchanged: their latest releases need
  Python 3.11+, and this patch keeps Python 3.10.

## [1.0.2] - 2026-10-27

### Added

- Python 3.15: tested in CI (every OS the tests run on) and listed in the package classifiers.
  Every dependency, test extras included, installs on it.

### Changed

- The README's cross-tool examples and the cross-tool tests use
  [Hardcharger](https://gizmodata.com/hardcharger) (GizmoData's DuckDB 2.0 server) instead of
  GizmoSQL: Oracle → Hardcharger streaming ingest, and `adbc_scanner` reads and writes run both
  inside a Hardcharger server and in an embedded DuckDB. Test settings are now
  `ADBC_ORACLE_TEST_HARDCHARGER_*`; without them the tests start a server from the `hardcharger`
  PyPI package (`pip install -e ".[test,hardcharger]"`). The driver itself is unchanged.
- README: `adbc_scanner` for DuckDB 2.0 and later (and so Hardcharger) installs from the Query.Farm
  extension repository; the community build refuses to load inside Hardcharger. Batch sizing is
  described against Hardcharger's 64 MiB ingest message limit.
- Dependencies: arrow-adbc Go 1.13.0 (was 1.11.0), arrow-go 18.8.0 (was 18.7.0) and their
  dependencies; the Go module now needs Go 1.26 (arrow-adbc 1.13 does). CI builds with Go 1.27.3.
  arrow-go 18.8 refuses a `Z` suffix on a time-zone-less timestamp in `array.FromJSON`; one test
  fixture changed (the driver doesn't parse timestamps from JSON).
  Test extras: pytest 9.1, DuckDB 2.0, pandas 3.1 and shapely 2.2 (Python 3.10 keeps their last
  releases for it). The runtime floors (`adbc-driver-manager`, `pyarrow`) are unchanged, so the
  wheel still installs next to older versions.
- CI cancels a superseded run on the same branch (never a tag run).

## [1.0.1] - 2026-10-15

### Fixed

- **The Linux wheels load on glibc 2.17 and newer, as their `manylinux2014` tag claims.** The Linux
  c-shared library was built with the GitHub runner's gcc and needed `GLIBC_2.32`, so on Oracle Linux 8
  (glibc 2.28), the database servers this driver is for, pip installed the wheel and the driver then
  failed to load (`version 'GLIBC_2.32' not found`). It is now built with `zig cc` targeting glibc 2.17.
  Same Go code; only the C toolchain differs. The release's `.so` artifacts carry the portable build.

### Changed

- CI fails if the Linux library needs a glibc newer than 2.17, and installs both Linux wheels on Oracle
  Linux 8 (x86_64, arm64) and loads the driver there before anything is published.
- The DuckDB `adbc_scanner` example (README) and its tests use the extension's current API: `adbc_scan` /
  `adbc_insert` take the attached database's name (`'ora'`). The extension removed `adbc_connect` /
  `adbc_disconnect`, so the old example and the cross-tool tests failed.
- CI builds with Go 1.26.9 (was 1.25; the module's `go 1.25.0` floor is unchanged) and pins `goimports`
  (x/tools v0.52.0): an unpinned `@latest` started requiring Go 1.26 and broke every Linux Python test job.

## [1.0.0] - 2026-09-01

First stable release. 🎉 Identical to 1.0.0-rc1 apart from the version
metadata (classifier is now Production/Stable) — see the 1.0.0-rc1 notes
below for everything new since 0.4.2, headlined by:

- **Security:** `adbc.oracle.nne=required` fails closed (never sends
  cleartext), with new read-only `nne_active` / `nne_algorithms`
  connection options to verify an encrypted session.
- **Streaming DX:** `batch_bytes` defaults to 8 MiB, so record batches
  fit under Flight SQL's 16 MiB gRPC cap out of the box.

## [1.0.0-rc1] - 2026-09-01

First release candidate for 1.0.0 — the driver leaves alpha status.

### Fixed

- **Security — `adbc.oracle.nne=required` now fails closed.** Previously
  the client-side NNE level was only honored when the *server* demanded
  encryption: against a server that did not, `required` silently
  proceeded with application data (SQL text and results) in cleartext.
  Now `required` (and `requested`) drive the Advanced Networking
  negotiation from the client on the first connect pass, and `required`
  refuses the connection (ORA-12660) whenever the negotiated protection
  is missing — including against servers with
  `SQLNET.ENCRYPTION_SERVER=REJECTED`. A TLS (`tcps`) channel satisfies
  the requirement. Verified at the wire-byte level in the test suite
  (`test_nne.py::test_wire_bytes_are_encrypted_with_required`).

### Added

- Read-only connection options `adbc.oracle.nne_active` ("true"/"false")
  and `adbc.oracle.nne_algorithms` (e.g. "AES256,SHA512") so an
  encrypted session is independently verifiable from the application.
  With NNE actually active, Oracle's own
  `V$SESSION_CONNECT_INFO.NETWORK_SERVICE_BANNER` also lists the
  encryption / crypto-checksumming service adapters.
- NNE test coverage: a client-level × server-level negotiation matrix
  (accepted/requested/required/rejected against accepting, requiring and
  rejecting servers), fail-closed unit tests, and a TCP-proxy test that
  asserts no SQL text or result bytes appear on the wire under
  `required`.

### Changed

- `adbc.oracle.batch_bytes` now defaults to **8 MiB** (was unlimited),
  so a single Arrow record batch always fits under common transport
  limits — in particular Flight SQL's 16 MiB gRPC message cap when
  streaming wide tables straight into `adbc_ingest` on another driver.
  Batches are still capped at `batch_size` rows (default 65,536); set
  `batch_bytes=0` to restore unlimited batches.
- `adbc.oracle.nne=requested` now negotiates encryption with any server
  that accepts it (previously it behaved like `accepted`).
- Statement cancellation falls back to in-band interrupt markers (not
  TCP out-of-band breaks) on connections configured with
  `nne=requested`/`required`, matching existing behavior on
  NNE-required servers.
- README: document tuning `batch_bytes` / `batch_size` when streaming wide
  tables into GizmoSQL (the Flight SQL client's 16 MiB gRPC message cap).

## [0.4.2] - 2026-08-27

### Added

- README + `test_adbc_scanner.py`: document and test writing back to Oracle
  through the attached catalog with plain SQL (`USE ora.<schema>;
  CREATE TABLE ... AS SELECT`), in addition to the existing `adbc_insert()`
  function push — both verified against a live Oracle server.

## [0.4.1] - 2026-08-27

### Added

- `NOTICE` file with full third-party attribution and an explicit
  independent-reimplementation / trademark statement; it now ships inside
  the wheel (`dist-info/licenses/`).
- README "Provenance & licensing" and "Trademarks" sections and a
  not-affiliated-with-Oracle disclaimer.

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

[Unreleased]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.4.2...HEAD
[0.4.2]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/gizmodata/adbc-driver-oracle/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/gizmodata/adbc-driver-oracle/releases/tag/v0.1.0
