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
pkg/oracle/        cgo c-shared wrapper — entrypoint AdbcDriverOracleInit (deprecated alias OracleDriverInit)
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

Build the c-shared lib for Python tests with `make -C pkg/oracle` (it runs
`scripts/build_lib.sh`, which limits the exports to `Adbc*` plus the deprecated
`OracleDriverInit` alias via `pkg/oracle/exports_*`; check a build with
`go run ./scripts/checkexports -allow OracleDriverInit <lib>`), then
`ADBC_ORACLE_LIBRARY=$PWD/pkg/oracle/libadbc_driver_oracle.dylib pip install -e ".[test,hardcharger]"`
and `pytest -v`. After rebuilding the lib, re-copy it into the editable
package: `cp pkg/oracle/libadbc_driver_oracle.dylib python/adbc_driver_oracle/`.

Cross-tool tests (test-only dependencies — **no Hardcharger code in the
driver**): `test_oracle_to_hardcharger.py` and the `[hardcharger]` cases of
`test_adbc_scanner.py` need a [Hardcharger](https://gizmodata.com/hardcharger)
2.1+ server. With `ADBC_ORACLE_TEST_HARDCHARGER_URI` set (+ `_USERNAME` /
`_PASSWORD`, default `hardcharger`/`hardcharger`, and `_DRIVER_PATH`: this
driver's library as the *server* sees it) they use that server and fail if it
doesn't answer; otherwise they start one from the `hardcharger` PyPI package
(the `[hardcharger]` extra, Python 3.11+). CI runs
`ghcr.io/gizmodata/hardcharger:latest` with `--network host` and
`pkg/oracle` mounted at `/drivers`. `test_adbc_scanner.py` runs every case in
Hardcharger and in an embedded DuckDB; on DuckDB 2.0+ (every Hardcharger) the
extension installs from the Query.Farm repository, on 1.x from community.

Go-level integration tests: `go test ./...` — `internal/ttc` has raw
wire-protocol tests, `driver/oracle` has ADBC-level ones; both read
`ADBC_ORACLE_TEST_URI` and skip without a listener on localhost:1521 when it
is unset (`internal/ttc` fails instead when it is set and nothing answers;
`ADBC_ORACLE_TEST_TRACE=1` logs its wire trace).

Test settings use `ADBC_ORACLE_*` names only (`ADBC_ORACLE_TEST_*` for new
ones), never a bare `ORACLE_*`: a developer's own Oracle environment must
never reach a test. (`ORACLE_PASSWORD` above is the gvenzl image's own
setting.)

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

- Not supported: REF CURSOR values / VECTOR / BFILE / LOB-typed OUT
  binds, binding or ingesting object types, Kerberos/RADIUS network
  auth, DRCP, password-protected wallet keys.
- Native Network Encryption (`internal/tns/ano.go`, `security.go`):
  who initiates depends on the client level. At `accepted` (default)
  two-phase connect — first pass sends DisableNA (proven safe for all
  servers, zero regression); if the ACCEPT reports NA-required and the
  user allows NNE, close and reconnect with NA enabled. At
  `requested`/`required` the *first* pass enables NA and always runs the
  ANO negotiation after ACCEPT — servers negotiate even without setting
  the NA flag in ACCEPT (verified: default/ACCEPTED servers encrypt for
  a requesting client; REJECTED servers answer the encryption service
  with an ORA-12660 error code, no hang). `required` additionally fails
  closed in `Conn.verifyNNERequirement` (before authenticate) if the
  transport ended up without the required cipher/hash — never assume the
  negotiation happened. `Conn.NNEInfo()` → driver read-only options
  `adbc.oracle.nne_active`/`nne_algorithms`. Negotiation then DH key
  exchange installs packet AES-CBC + keyed-hash protection on the
  transport. Must reset `caps` before an ANO reconnect so the fresh
  CONNECT is 2-byte-framed. The checksum keystream evolves per DATA
  packet and re-inits at every marker/reset boundary (`Security.Reset()`
  in `Conn.reset()`, *before* any post-reset packet is read/unwrapped) —
  getting that ordering wrong desyncs on the first error. Markers/control
  packets are never encrypted. **OOB breaks are disabled for any pass
  that may end up encrypted** (including client `requested`/`required`) —
  a raw TCP urgent byte bypasses the encrypted channel and some servers
  reset/hang on it. Cancellation over NNE uses in-band interrupt markers.
  Local NNE containers: `oracle-nne` (:1523, sqlnet REQUIRED) and
  `oracle-nne-rejected` (:1524, sqlnet REJECTED) — same gvenzl image,
  set up by `scripts/setup_nne_server.sh <container> REQUIRED|REJECTED`
  (`SQLNET.ENCRYPTION_SERVER` / `SQLNET.CRYPTO_CHECKSUM_SERVER` in its
  `sqlnet.ora`; CI's Linux go-test leg runs both as services). Tests:
  `internal/ttc` `TestLiveNNE*` (`ADBC_ORACLE_NNE_URI`,
  `ADBC_ORACLE_NNE_REJECTED_URI`; they fail when set and unreachable),
  python `test_nne.py` (`ADBC_ORACLE_NNE_URI`; the wire-bytes proxy test
  runs against the standard container).
- Object types (`internal/ttc/objtype.go`, `objimage.go`, driver
  `objects.go`): type metadata comes from ALL_TYPES / ALL_TYPE_ATTRS /
  ALL_COLL_TYPES (not dbms_pickler — it needs a REF CURSOR OUT bind),
  cached per connection. Column images are buffered raw in `arrowSink`
  and decoded in `flushObjects` when a batch is materialised, because
  dictionary lookups can't run mid-response. BOOLEAN attrs are 4-byte
  ints (first byte set = TRUE); LOB attributes are fetched with the LOB
  READ op (`Conn.ReadLOB`). SDO_GEOMETRY → WKB lives in `objects.go`.
  Spatial isn't in the slim container: use `gvenzl/oracle-free:23-faststart`
  (`ADBC_ORACLE_SPATIAL_URI`; CI now runs that image).
- Cancellation: `Conn.Cancel()` / ctx cancellation sends a TCP
  out-of-band byte (Unix, plain TCP; the OOB check happens right after
  ACCEPT) or an INTERRUPT marker otherwise; the server answers with
  markers that `reset()` consumes and the call fails with ORA-01013.
  Oracle only checks for breaks at SQL checkpoints — PL/SQL sleeps run
  to completion (verified identical with python-oracledb thin).
- PL/SQL OUT / IN OUT binds: the I/O vector gives directions, ROW_DATA
  then carries the out values (`Statement.OutBindValue`); implicit
  result sets (`DBMS_SQL.RETURN_RESULT`) arrive as message 27 with a
  describe + cursor id; child cursors fetch via an execute message
  without SQL/EXECUTE (`Statement.isChild`).
- `adbc_driver_manager` (Python) does not forward `table_types_filter`
  to drivers (TODO in its `_lib.pyx`); the Go driver filters correctly.
- Unknown-precision NUMBER (expressions, `COUNT(*)`) maps to `float64`
  in `number_mode=auto`; `decimal`/`string` modes are exact.

## Password verifiers

`internal/ttc/auth.go` handles 12c (0x4815, PBKDF2/SHA-512), 11g (0xb152 /
0x1b25, SHA-1) and, only with `adbc.oracle.allow_10g_verifier=true`, 10g
(0x939, O3LOGON: DES hash of upper(user||password) in UTF-16BE, ported from
go-ora, see NOTICE). Without the option a 0x939 challenge fails in
`authMessage.write` before any key material is derived (`Err10gVerifierNotAllowed`,
mapped to UNAUTHENTICATED): a MITM on plain TCP could otherwise downgrade a 12c
account's login to a cheaply crackable 10g exchange. 21c servers send
AUTH_PBKDF2_CSK_SALT for 10g too (PBKDF2 combo key); pre-12c servers would use
the MD5 path (unit-tested only).

23ai/26ai refuse 10g verifiers (`IDENTIFIED BY VALUES '<16 hex>'` → ORA-02153),
so the live tests use `gvenzl/oracle-xe:21-slim-faststart` (amd64; runs under
emulation on Apple Silicon given enough Docker memory):

```bash
docker run --name oracle-verifiers -d -p 1522:1521 -e ORACLE_PASSWORD=tiger gvenzl/oracle-xe:21-slim-faststart
scripts/setup_verifier_users.sh oracle-verifiers   # ADBC_TEST_10G/_11G/_12C, one verifier each
ADBC_ORACLE_TEST_10G_URI=oracle://adbc_test_10g:Tiger10g@127.0.0.1:1522/XEPDB1 \
ADBC_ORACLE_TEST_11G_URI=oracle://adbc_test_11g:Tiger11g@127.0.0.1:1522/XEPDB1 \
ADBC_ORACLE_TEST_12C_URI=oracle://adbc_test_12c:Tiger12c@127.0.0.1:1522/XEPDB1 \
  go test -run 'TestLive1' ./driver/oracle/
```

They skip without those URIs unless `ADBC_ORACLE_TEST_VERIFIERS_REQUIRED=true`
(CI's Linux go-test leg, which runs a second service container, `oracle_xe21`).

## Test battery

`python/tests/test_smoke.py` mirrors the README (quickstart, streaming,
ingest modes, transactions, qmark binds), plus DBAPI basics, type
mapping (incl. 23ai JSON/BOOLEAN/INTERVAL/LTZ), NUMBER edge cases and
`number_mode`, LOBs, bulk-ingest typed round trips, metadata
(`get_info`/`get_table_types`/`get_table_schema`/`get_objects` with
constraints), connection options; `test_profiles.py` / `test_manifest.py`
cover driver manifests + connection profiles. Keep it one-for-one with
the Quack / hardcharger-adbc batteries when adding features.

## Conformance: `validation/`

The [adbc-drivers.org validation suite](https://github.com/adbc-drivers/validation) runs
against the c-shared library (`validation/README.md` has the commands; CI job
`validation`, which gates wheels/PyPI/release). It is a **pinned test-only dependency**
(`validation/requirements.txt`, git commit): don't add the adbc-drivers pixi /
adbc-gen-workflow tooling, and don't register or open anything with that org.

- `tests/oracle.py` — `DriverQuirks`: features, `quote_one_identifier` (mirrors the
  driver's `identFor`: simple names upper-cased), Oracle DDL for constraints, statement
  splitting, the long-values ingest options. `tests/conftest.py` creates the secondary
  schema with `ADBC_ORACLE_TEST_ADMIN_URI`, fails (never skips) on missing settings or
  zero passed tests, and accepts an actual column named `name.upper()` only where `name`
  is an identifier Oracle folds.
- `queries/oracle/` — per-case overrides (Oracle SQL; expected results from the README
  type mapping, with a caveat per difference). Declare something unsupported only when
  Oracle (or ADBC semantics for it) lacks it, never to hide a driver bug.
- Bumping the suite: change the commit in `requirements.txt`, run it, review new cases.

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

## Provenance / legal (keep this posture)

Independent, from-scratch Go reimplementation of the Oracle TNS/TTC protocol.
No Oracle source code, no Oracle libraries, no confidential spec or binary
disassembly — everything traces to publicly, openly licensed sources
(`python-oracledb` thin, Apache-2.0/UPL, Oracle's own; `go-ora`, MIT; Apache
Arrow ADBC/Go, Apache-2.0). Attribution lives in `NOTICE` (shipped in the
wheel via `license-files`/`MANIFEST.in`). Keep the public framing on value
("no Instant Client, pip-installable, Arrow-native") and clean provenance:
an independent Go implementation built from openly-licensed references. Frame
NNE affirmatively — a client-side implementation of Oracle's documented
Advanced Networking negotiation, where client and server agree on cipher and
session keys via Diffie-Hellman; we implement the client half of that
handshake. Keep it at "works with servers requiring Native Network
Encryption." Never use Oracle's logo or imply endorsement; the
README carries the trademark disclaimer. Don't commit customer creds or
live-customer tests.
