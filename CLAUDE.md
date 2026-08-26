# CLAUDE.md

Project notes for Claude and other contributors.

## What this is

A Go-based [Apache Arrow ADBC](https://arrow.apache.org/adbc/) driver
for Oracle Database that speaks the native TNS/TTC wire protocol
directly — **no Oracle Client / Instant Client libraries** — distributed
as a Go module and as a Python wheel (`pip install adbc-driver-oracle`).

Sibling repos with the same shape (copy conventions from them, not from
upstream): `adbc-driver-db2` (pure-Go DRDA — the closest analogue, also
a from-scratch wire protocol) and `adbc-driver-quack`.

## Reference material

- `/tmp/adbc-ref/python-oracledb/src/oracledb/impl/thin/` — Oracle's
  own pure-Python "thin mode" driver (Apache-2.0/UPL). **This is the
  authoritative protocol reference.** Key files: `packet.pyx` (TNS
  framing + read/write buffer), `protocol.pyx` (connect/negotiate),
  `messages/base.pyx` (TTC message state machine, row decoding),
  `messages/auth.pyx` (O5LOGON), `messages/execute.pyx` /
  `messages/fetch.pyx`, `messages/data_types.pyx`, `capabilities.pyx`,
  `constants.pxi`, `crypto.pyx`. Number/date codecs live in
  `../base/buffer.pyx`.
- `/tmp/adbc-ref/go-ora/v2/` — community pure-Go driver (MIT). Useful
  as a second opinion in Go (`network/`, `converters/`, `command.go`)
  but its type handling is less complete than python-oracledb's.
- `/Users/philip/LocalOnly/git/arrow-adbc` — ADBC framework. Read
  `go/adbc/adbc.go` for interface contracts, `go/adbc/driver/flightsql/`
  for the canonical Go driver shape, `go/adbc/validation/` for the
  conformance suite, `go/adbc/pkg/_tmpl/` for the cgo wrapper templates.

## Layout

```
internal/tns/      TNS packet layer (CONNECT/ACCEPT/REDIRECT/DATA/MARKER), TLS, SDU
internal/ttc/      TTC messages: negotiation, auth, open/execute/fetch/close, errors
internal/oratype/  NUMBER, DATE/TIMESTAMP(TZ/LTZ), INTERVAL, RAW, ROWID, LOB codecs
driver/oracle/     ADBC Driver/Database/Connection/Statement, options, Arrow conv, metadata, ingest
pkg/oracle/        cgo c-shared wrapper — entrypoint OracleDriverInit (alias AdbcDriverOracleInit)
python/            adbc_driver_oracle wheel sources + tests
```

`pkg/oracle/driver.go` expects `oracle.NewDriver(memory.Allocator) adbc.Driver`
from `driver/oracle`.

## Local test database

```bash
docker run --name oracle-adbc-test -d -p 1521:1521 \
  -e ORACLE_PASSWORD=tiger -e APP_USER=scott -e APP_USER_PASSWORD=tiger \
  gvenzl/oracle-free:23-slim-faststart
```

→ `oracle://scott:tiger@localhost:1521/FREEPDB1` (SYSTEM password is
also `tiger`). Integration tests (Go and Python) read
`ADBC_ORACLE_TEST_URI`; when it is unset they fall back to that URI and
**skip** if nothing is listening on the host:port. CI runs the same
container as a GitHub Actions service on the Linux legs only.

Build the c-shared lib for Python tests with `make -C pkg/oracle`, then
`ADBC_ORACLE_LIBRARY=$PWD/pkg/oracle/libadbc_driver_oracle.dylib pip install -e ".[test,gizmosql]"`
and `pytest -v`. After rebuilding the lib, re-copy it into the editable
package: `cp pkg/oracle/libadbc_driver_oracle.dylib python/adbc_driver_oracle/`.

Cross-tool tests (test-only dependencies — **no GizmoSQL code in the
driver**): `test_oracle_to_gizmosql.py` needs `GIZMOSQL_URI` (+
`GIZMOSQL_TOKEN` or `GIZMOSQL_USERNAME`/`GIZMOSQL_PASSWORD`), e.g.
`docker run -d --name gizmosql --init -p 31337:31337 -e TLS_ENABLED=0 -e GIZMOSQL_PASSWORD=gizmosql_password gizmodata/gizmosql:latest`
→ `GIZMOSQL_URI=gizmosql://localhost:31337?transport=tcp`.
`test_adbc_scanner.py` needs the `duckdb` package and internet access
for `INSTALL adbc_scanner FROM community`; it exercises `adbc_scan`
(pull) and `adbc_insert` (push) against Oracle through our c-shared lib.

Go-level integration tests: `go test ./...` — `internal/ttc` has raw
wire-protocol tests, `driver/oracle` has ADBC-level ones; both skip
without a listener on localhost:1521.

## How the wire layer is put together

- `internal/tns`: `Transport` (TCP/TLS, packet framing), `ReadBuffer` /
  `WriteBuffer` (TTC integer encodings ub1/ub2/ub4/ub8/sb*, length-
  prefixed and chunked byte strings, SDU-sized packet splitting) with a
  **sticky error** (read/write methods never return errors; check
  `Err()`), `Capabilities` (compile/runtime caps negotiated with the
  server).
- `internal/ttc`: `Conn` owns the handshake (`connect.go` →
  `protocol.go` (+ `datatypes_table.go`) → `auth.go` O5LOGON two-phase
  or 23ai fast-auth), `processMessage` (send request, read the whole
  response, marker/reset recovery on errors, piggybacks for cursor
  close / current schema / end-to-end attrs). `execute.go` is the
  statement engine: `Statement.Execute` (function 94 with PARSE /
  EXECUTE / FETCH / DEFINE options), `Fetch` (function 5), array binds
  (`BindColumn.Value(row)` returns wire-encoded bytes), row decoding
  into a `RowSink` (raw wire bytes per column; the driver's
  `arrowSink` decodes straight into Arrow builders).
- LOB columns are never fetched as locators: describe info marks the
  statement `requiresDefine`, the first execute's rows are discarded and
  a second execute re-defines CLOB→LONG / BLOB→LONG RAW so data arrives
  inline. JSON stays native (OSON, prefetched via the define pass) and is
  decoded by `oratype.DecodeOSONToJSON`. Row data uses a duplicate-value
  bit vector (`AppendDuplicate`).
- Session TIME_ZONE is set to UTC at logon (`AUTH_ALTER_SESSION`) so
  `TIMESTAMP WITH LOCAL TIME ZONE` arrives as UTC. `TIMESTAMP WITH TIME
  ZONE` is transmitted as UTC wall-clock + offset; the offset is
  display-only, so decode the fields as UTC (do **not** subtract it).
- Go dispatch gotcha: `baseMessage` methods are not virtual. Hooks that
  a concrete message overrides (`processReturnParameters`, `finish`) are
  resolved through `baseMessage.self` / interface assertions in
  `process()` and `Conn.processMessage`.
- Oracle NUMBER exponent bytes must be handled with wrapping int8
  arithmetic (see `oratype.DecodeNumber`); the encoder appends the 102
  sentinel for negatives.

## Known limitations / open items

- Not supported: Native Network Encryption (server must not *require*
  it; TLS is the alternative), object types / XMLTYPE / REF CURSOR /
  implicit result sets / VECTOR / BFILE / PL/SQL OUT binds, Kerberos,
  DRCP, password-protected wallet keys, statement cancellation and call
  timeouts.
- `adbc_driver_manager` (Python) does not forward `table_types_filter`
  to drivers (TODO in its `_lib.pyx`); the Go driver filters correctly.
- Unknown-precision NUMBER (expressions, `COUNT(*)`) maps to `float64`
  in `number_mode=auto`; `decimal`/`string` modes are exact.

## Test battery

`python/tests/test_smoke.py` mirrors the README (quickstart, streaming,
ingest modes, transactions, qmark binds), plus DBAPI basics, type
mapping (incl. 23ai JSON/BOOLEAN/INTERVAL/LTZ), NUMBER edge cases and
`number_mode`, LOBs, bulk-ingest typed round trips, metadata
(`get_info`/`get_table_types`/`get_table_schema`/`get_objects` with
constraints), connection options; `test_profiles.py` / `test_manifest.py`
cover driver manifests + connection profiles. Keep it one-for-one with
the Quack / gizmosql-adbc batteries when adding features.

## Conventions

- **Go 1.25 floor** (per `go.mod`). Use generics where they help;
  avoid `any` when a concrete type fits.
- **No `panic` across the cgo boundary.** The wrapper mirrors
  flightsql's `globalPoison` + `defer/recover`; keep driver code from
  panicking regardless.
- **Apache Arrow Go pin: v18**, arrow-adbc Go v1.11. Match the siblings.
- **Option naming:** `adbc.oracle.<noun>` (e.g. `adbc.oracle.tls`,
  `adbc.oracle.batch_size`). Standard ADBC keys (`uri`, `username`,
  `password`, ingest options) are used where they exist.
- **DriverInfoName / vendor string:** "ADBC Oracle Driver - Go" /
  "Oracle Database".
- **Python floor:** `>=3.10`; pyarrow `>=14.0.1`; adbc-driver-manager
  `>=1.11.0` (driver manifests + connection profiles).
- **Python style:** keyword arguments at call sites wherever the callee
  accepts them (`cur.adbc_ingest(table_name=..., data=..., mode=...)`).
- **Oracle SQL in tests:** `SELECT ... FROM DUAL`, `DROP TABLE x PURGE`,
  identifiers are upper-cased by the server unless quoted; test tables
  are prefixed `ADBC_TEST_`.
- **Releases:** semver `vX.Y.Z` tags; keep `driver/oracle/version.go`,
  `python/adbc_driver_oracle/_version.py`, and `CHANGELOG.md` in sync.
  Tag pushes build wheels, publish to PyPI, and create a GitHub Release
  with the CHANGELOG section as notes.
