# adbc-driver-oracle

**An [Apache Arrow ADBC](https://arrow.apache.org/adbc/) driver for [Oracle Database](https://www.oracle.com/database/) — pure Go, no Oracle Client libraries required.**

> An independent, open-source project. Not affiliated with, endorsed by, or sponsored by Oracle Corporation. "Oracle" and "Oracle Database" are trademarks of Oracle Corporation, used here only to describe compatibility.

[<img src="https://img.shields.io/badge/GitHub-gizmodata%2Fadbc--driver--oracle-blue.svg?logo=Github">](https://github.com/gizmodata/adbc-driver-oracle)
[![CI](https://github.com/gizmodata/adbc-driver-oracle/actions/workflows/ci.yml/badge.svg)](https://github.com/gizmodata/adbc-driver-oracle/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/gizmodata/adbc-driver-oracle.svg)](https://pkg.go.dev/github.com/gizmodata/adbc-driver-oracle)
[![Go Version](https://img.shields.io/github/go-mod/go-version/gizmodata/adbc-driver-oracle)](go.mod)
[![Supported Python Versions](https://img.shields.io/pypi/pyversions/adbc-driver-oracle)](https://pypi.org/project/adbc-driver-oracle/)
[![PyPI version](https://badge.fury.io/py/adbc-driver-oracle.svg)](https://badge.fury.io/py/adbc-driver-oracle)
[![PyPI Downloads](https://img.shields.io/pepy/dt/adbc-driver-oracle.svg)](https://pypi.org/project/adbc-driver-oracle/)
[![License](https://img.shields.io/github/license/gizmodata/adbc-driver-oracle)](LICENSE)

Speaks Oracle's native TNS/TTC wire protocol directly from Go and returns
Apache Arrow `RecordBatch`es straight from the result set — the way
`python-oracledb`'s "thin mode" does, but for the whole ADBC ecosystem.
**No Instant Client, no `ORACLE_HOME`, no `LD_LIBRARY_PATH`.** Just
`pip install`. Supports the standard ADBC bulk-ingest path
(`Statement.BindStream` → array-bind `INSERT`) for fast Arrow → Oracle loads.

Distributed as:
- a Go module — `github.com/gizmodata/adbc-driver-oracle`
- a `pip install adbc-driver-oracle` wheel for Python (macOS / Linux / Windows × x64 / arm64)
- a c-shared library (`libadbc_driver_oracle.{so,dylib,dll}`) attached to each
  GitHub Release for C / C++ / Rust / R / driver-manifest consumers

> **Status:** Alpha. Tested against Oracle Database 23ai Free; the wire
> protocol targets Oracle 12.1 and later (the same range as
> python-oracledb thin mode). See [Limitations](#limitations) for what
> is not covered yet.

## Quickstart

### 1. Have an Oracle Database handy

For local development the [gvenzl/oracle-free](https://github.com/gvenzl/oci-oracle-free)
container is the fastest route (x86_64 and arm64):

```bash
docker run --name oracle-adbc-test -d -p 1521:1521 \
  -e ORACLE_PASSWORD=tiger -e APP_USER=scott -e APP_USER_PASSWORD=tiger \
  gvenzl/oracle-free:23-slim-faststart
```

That gives you `oracle://scott:tiger@localhost:1521/FREEPDB1`.

### 2. Install the driver

**Python:**

```bash
pip install adbc-driver-oracle
```

**Go:**

```bash
go get github.com/gizmodata/adbc-driver-oracle@latest
```

### 3. Connect and query

```python
import adbc_driver_oracle.dbapi as oracle
import pyarrow

with oracle.connect(
    uri="oracle://scott:tiger@localhost:1521/FREEPDB1",
) as conn, conn.cursor() as cur:
    cur.execute("SELECT 42 AS answer, 'hello oracle' AS greeting FROM DUAL")
    table: pyarrow.Table = cur.fetch_arrow_table()
    print(table)
```

The result is a real `pyarrow.Table` — pass it straight to Polars, Pandas,
DuckDB, ibis, or anything else that consumes Arrow:

```python
import polars as pl
df = pl.from_arrow(table)
```

Prefer to keep credentials out of the URI? Pass them as options:

```python
with oracle.connect(
    uri="oracle://localhost:1521/FREEPDB1",
    db_kwargs={"username": "scott", "password": "tiger"},
) as conn:
    ...
```

#### Alternative: drive `adbc_driver_manager` directly

If you prefer the generic [ADBC driver manager](https://arrow.apache.org/adbc/current/python/driver_manager.html)
idiom — passing the driver to `adbc_driver_manager.dbapi.connect` rather
than going through our wrapper — point at the bundled shared library via
`_driver_path()`:

```python
from adbc_driver_manager import dbapi
import adbc_driver_oracle

with dbapi.connect(
    driver=adbc_driver_oracle._driver_path(),
    entrypoint="OracleDriverInit",
    db_kwargs={"uri": "oracle://scott:tiger@localhost:1521/FREEPDB1"},
) as conn, conn.cursor() as cur:
    cur.execute("SELECT 42 AS answer FROM DUAL")
    table = cur.fetch_arrow_table()
```

### Streaming large result sets

`Cursor.fetch_record_batch()` returns a `pyarrow.RecordBatchReader` that
pulls rows from the server one batch at a time. Memory stays bounded by
`adbc.oracle.batch_size` even when the result is millions of rows:

```python
with conn.cursor() as cur:
    cur.execute("SELECT * FROM sales.orders")  # arbitrary size
    reader = cur.fetch_record_batch()
    for batch in reader:
        process(batch)
```

### Oracle → GizmoSQL (or any ADBC target), ADBC to ADBC

The reader above can be handed straight to another driver's bulk ingest,
so a table moves from Oracle into [GizmoSQL](https://gizmodata.com/gizmosql)
without ever being materialised on the client — no pandas, no ODBC, no
Oracle Client:

```python
import adbc_driver_oracle.dbapi as oracle
import adbc_driver_gizmosql.dbapi as gizmosql

with oracle.connect(uri=oracle_uri) as src, \
     gizmosql.connect(gizmosql_uri, username="token", password=token) as dst:
    with src.cursor() as s, dst.cursor() as d:
        s.execute("SELECT * FROM sales.orders")
        rows = d.adbc_ingest(table_name="orders", data=s.fetch_record_batch(), mode="replace")
        dst.commit()
        print(f"Loaded {rows:,} rows")
```

(`python/tests/test_oracle_to_gizmosql.py` runs exactly this against a
live Oracle + GizmoSQL pair in CI; GizmoSQL is a test-only dependency —
the driver itself has no GizmoSQL code.)

### DuckDB / GizmoSQL ↔ Oracle via `adbc_scanner`

Because the driver is a plain ADBC c-shared library, DuckDB — and
therefore GizmoSQL, which embeds DuckDB — can talk to Oracle *live*
through the [`adbc_scanner`](https://github.com/gizmodata/adbc_scanner)
community extension, in both directions:

```sql
INSTALL adbc_scanner FROM community;
LOAD adbc_scanner;

-- credentials once, in a DuckDB secret
CREATE SECRET ora_secret (
    TYPE adbc,
    SCOPE 'oracle://db.example.com:1521/PROD',
    driver 'oracle',                      -- by name after `python -m adbc_driver_oracle install-manifest`,
                                          -- or a path: '/path/to/libadbc_driver_oracle.so'
    uri 'oracle://db.example.com:1521/PROD',
    username 'app',
    password 's3cret'
);

-- pull: Oracle as an attached catalog (projection + filter pushdown)
ATTACH 'oracle://db.example.com:1521/PROD' AS ora (TYPE adbc);
SELECT * FROM ora.SALES.ORDERS WHERE AMT > 100;
SELECT o.ORDER_ID, c.name FROM ora.SALES.ORDERS o JOIN customers c ON c.id = o.CUST_ID;

-- arbitrary Oracle SQL, and push in the other direction, via the same secret
SET VARIABLE ora = adbc_connect({'secret': 'ora_secret'});
SELECT * FROM adbc_scan(getvariable('ora')::BIGINT, 'SELECT * FROM sales.orders WHERE ROWNUM <= 10');
SELECT * FROM adbc_insert(getvariable('ora')::BIGINT, 'ORDERS_COPY', (SELECT * FROM local_orders), mode := 'create');
```

`python/tests/test_adbc_scanner.py` covers both directions.

### DuckDB via connection profiles (the `adbc` community extension)

DuckDB's `adbc` community extension (`INSTALL adbc FROM community`; see the
[GizmoSQL guide](https://docs.gizmosql.com/adbc_duckdb_extension/))
resolves databases through ADBC connection profiles — no credentials in
your SQL — and additionally supports writes (`INSERT`, `COPY`,
`CREATE TABLE AS`) into the attached database through ADBC bulk ingest:

```sh
python -m adbc_driver_oracle install-manifest      # registers driver "oracle"
cat > ~/.config/adbc/profiles/prod.toml <<EOF        # macOS: ~/Library/Application Support/ADBC/Profiles/
profile_version = 1
driver = "oracle"

[Options]
uri = "oracle://db.example.com:1521/PROD"
username = "app"
password = "s3cret"
EOF
```

```sql
INSTALL adbc FROM community;
LOAD adbc;

SELECT * FROM read_adbc('profile://prod', 'SELECT * FROM sales.orders WHERE ROWNUM <= 10');

ATTACH 'profile://prod' AS ora (TYPE adbc);
USE ora.SALES;
SELECT COUNT(*) FROM ORDERS;
CREATE TABLE ORDERS_2024 AS SELECT * FROM memory.staged_orders;   -- bulk ingest into Oracle
```

`python/tests/test_duckdb_adbc_client.py` covers `read_adbc`, `ATTACH`,
and CTAS/INSERT. (The `adbc_scanner` extension above is the one to use
when you want predicate/projection pushdown on attached tables.)

### Bulk ingest (Arrow → Oracle)

```python
import pyarrow as pa
import adbc_driver_oracle.dbapi as oracle

table = pa.table({"id": [1, 2, 3], "name": ["alice", "bob", "carol"]})
with oracle.connect(
    uri="oracle://scott:tiger@localhost:1521/FREEPDB1",
    autocommit=True,  # ADBC connections are autocommit-OFF by default;
                      # opt in here so the ingest persists on close
) as conn, conn.cursor() as cur:
    # create_append: create CUSTOMERS from the Arrow schema if it
    # doesn't exist, then append via array-bind INSERT.
    cur.adbc_ingest(table_name="customers", data=table, mode="create_append")
```

> **Heads-up — autocommit is off by default.** Per the Python DB-API,
> `oracle.connect()` opens connections inside a transaction. Without the
> `autocommit=True` above (or an explicit `conn.commit()`), the append
> is **rolled back when the connection closes**. (Oracle DDL — the
> `CREATE TABLE` in the create-family modes — always commits implicitly.)

`mode` accepts the four standard ADBC ingest modes:

| mode            | behavior                                                  |
|-----------------|-----------------------------------------------------------|
| `create`        | create the table (errors if it already exists), then append — this is the default when `mode` is omitted |
| `append`        | append to an existing table (no DDL; errors if missing)   |
| `replace`       | drop the table if it exists, recreate it, then append     |
| `create_append` | create the table if it doesn't exist, then append         |

Table DDL for the create-family modes is generated from the Arrow schema
(see [Type mapping](#type-mapping)); simple column names are upper-cased
like unquoted Oracle identifiers, anything else is quoted verbatim. Pass
`db_schema_name=...` to target another schema. Rows are sent as
array-bound `INSERT`s, 5000 per round trip. Statement options
(`cur.adbc_statement.set_options(...)` / `stmt.SetOption` in Go):

| Statement option                     | Default    | Notes                                                          |
|--------------------------------------|------------|----------------------------------------------------------------|
| `adbc.oracle.ingest.batch_rows`      | `5000`     | Rows per array-bind `INSERT` round trip.                       |
| `adbc.oracle.ingest.varchar_length`  | `4000`     | `VARCHAR2(n)` length for Arrow string columns in generated DDL. |
| `adbc.oracle.ingest.string_type`     | `VARCHAR2` | `VARCHAR2`, `NVARCHAR2`, `CLOB` or `NCLOB` for string columns.  |
| `adbc.oracle.ingest.raw_length`      | `2000`     | `RAW(n)` length for Arrow binary columns.                      |
| `adbc.oracle.ingest.binary_type`     | `RAW`      | `RAW` or `BLOB` for binary columns.                            |
| `adbc.oracle.ingest.struct_type`     | `JSON`     | `JSON` (21c+), `CLOB`, `VARCHAR2` or `BLOB` for `list` / `struct` / `map` columns (stored as JSON text). |
| `adbc.oracle.ingest.tablespace`      | (default)  | `TABLESPACE` clause for tables created by ingest.              |

Values larger than the server's maximum `VARCHAR2`/`RAW` size are bound
as `LONG` / `LONG RAW`, so strings and blobs of any size load into
`CLOB` / `BLOB` columns.

### Transactions (autocommit off)

```python
import adbc_driver_oracle.dbapi as oracle

with oracle.connect(
    uri="oracle://scott:tiger@localhost:1521/FREEPDB1",
    autocommit=False,
) as conn, conn.cursor() as cur:
    cur.execute("INSERT INTO orders VALUES (1, 'pending')")
    cur.execute("INSERT INTO order_items VALUES (1, 'widget', 2)")
    conn.commit()  # both inserts persist atomically
```

### PL/SQL: OUT / IN OUT binds and implicit result sets

A PL/SQL block's `OUT` / `IN OUT` binds come back as a **one-row result
set** (the Arrow C Data Interface can't mutate bound input); each field carries
`ORACLE:parameter_type` = `OUT` / `IN OUT` metadata. Bind a typed
placeholder to choose the return type — an untyped `None` returns text:

```python
cur.execute(
    "BEGIN :doubled := :n * 2; :greeting := 'hello ' || :greeting; END;",
    pa.record_batch([pa.array([None], pa.int64()), pa.array([21], pa.int64()), pa.array(["world"])],
                    names=["doubled", "n", "greeting"]),
)
cur.fetch_arrow_table().to_pylist()   # [{'DOUBLED': 42, 'GREETING': 'hello world'}]
```

Cursors returned with `DBMS_SQL.RETURN_RESULT` stream back exactly like
a query (the first cursor; further ones are closed):

```python
cur.execute("""DECLARE c SYS_REFCURSOR; BEGIN
                 OPEN c FOR SELECT * FROM emp; DBMS_SQL.RETURN_RESULT(c); END;""")
table = cur.fetch_arrow_table()
```

### Object types, collections, XMLType and SDO_GEOMETRY

User-defined object types, VARRAYs and nested tables come back as **JSON
text** — attributes keyed by name, collections as arrays, LOB attributes
inlined, dates as ISO-8601 — so any Arrow consumer can read them without
Oracle-specific type knowledge. `XMLType` columns come back as XML text
and `MDSYS.SDO_GEOMETRY` as **GeoArrow WKB** (points, lines, polygons
incl. rectangles, multi-geometries and collections, 2D/3D/4D; circular
arcs are not supported). With `adbc.oracle.use_extension_types=true` the
fields are annotated with Arrow extension types (`arrow.json`,
`arrow.opaque` for XMLType, `geoarrow.wkb`) — pyarrow materialises them
automatically:

```python
with oracle.connect(uri=uri, db_kwargs={"adbc.oracle.use_extension_types": "true"}) as conn, conn.cursor() as cur:
    cur.execute("SELECT name, address, boundary FROM sites")   # address is an object type, boundary SDO_GEOMETRY
    table = cur.fetch_arrow_table()
    table.schema.field("BOUNDARY").type   # binary with geoarrow.wkb metadata -> shapely.from_wkb(...)
```

Nested Arrow columns (`list`, `struct`, `map`) are ingested as JSON text
into `JSON` columns (21c+; `adbc.oracle.ingest.struct_type` selects
`CLOB`, `VARCHAR2` or `BLOB` instead).

### Native Network Encryption (NNE)

The driver speaks Oracle's Advanced Networking Option, so it works with
servers that **require** Native Network Encryption and/or data integrity
checksumming — no Oracle Client and no TLS certificate wrangling. After
the connect handshake it runs the encryption / checksum negotiation
(Diffie-Hellman key exchange) and then AES-encrypts and checksums every
packet.

It is on by default at `accepted`: nothing changes for a server that
doesn't ask for it, and a server that requires it is transparently
negotiated (AES-256 / SHA-512 in the common case). Force it with
`adbc.oracle.nne=required` to guarantee the session is encrypted, or turn
it off with `rejected`:

```python
# Just works against a server with SQLNET.ENCRYPTION_SERVER=REQUIRED:
with oracle.connect(uri="oracle://user:pw@exadata-scan:1521/PROD") as conn, conn.cursor() as cur:
    cur.execute("SELECT * FROM sales.orders")   # every packet AES-encrypted + checksummed

# Refuse to connect unless the channel is encrypted:
oracle.connect(uri=uri, db_kwargs={"adbc.oracle.nne": "required"})
```

Supported: AES-128/192/256 encryption and MD5 / SHA-1 / SHA-256 / SHA-384
/ SHA-512 checksums (the driver offers only AES ciphers; legacy RC4/DES
are never used). Kerberos / RADIUS network authentication is not
supported — use password, TLS or token auth. Over a `tcps` (TLS)
connection NNE stays off by default since the channel is already
encrypted.

### Cancellation

`cursor.adbc_cancel()` (or a cancelled/expired `context.Context` in Go)
interrupts the running server call; the statement fails with
`ORA-01013` and the connection stays usable. Cancellation uses TCP
out-of-band data on Linux/macOS (in-band markers on Windows and over
TLS), the same mechanism as python-oracledb.

### Parameter binding

Positional `?` placeholders are rewritten to Oracle's `:1`, `:2`, …
bind variables; native `:name` / `:1` styles pass through untouched:

```python
cur.execute("SELECT ename, sal FROM emp WHERE deptno = ? AND sal > ?", (10, 1500))
```

## Connection URL

```
oracle://[user[:password]@]host[:port]/SERVICE_NAME[?option=value...]
oracle://[user[:password]@]host[:port]?sid=ORCL
```

Oracle [Easy Connect](https://docs.oracle.com/en/database/oracle/oracle-database/23/netag/configuring-naming-methods.html#GUID-B0437826-43C1-49EC-A94D-B650B6A4A6EE)
strings (`host:port/service`) and full `(DESCRIPTION=...)` TNS connect
descriptors are accepted in place of the `oracle://` form.

| Option                          | Default   | Notes                                                                 |
|---------------------------------|-----------|-----------------------------------------------------------------------|
| `adbc.uri`                      | —         | Pass as the `uri=` kwarg to `oracle.connect`.                         |
| `username` / `password`         | (URI)     | Standard ADBC credential options; override the URI's `user:password`. |
| `adbc.oracle.host`              | (URI)     | Database host.                                                        |
| `adbc.oracle.port`              | `1521`    | Listener port.                                                        |
| `adbc.oracle.service_name`      | (URI)     | Service name (e.g. `FREEPDB1`).                                       |
| `adbc.oracle.sid`               | (none)    | SID, as an alternative to a service name.                             |
| `adbc.oracle.tls`               | `false`   | `true` → TLS (`tcps`) transport.                                      |
| `adbc.oracle.tls.ca_cert`       | (none)    | PEM CA bundle for verifying the server certificate.                   |
| `adbc.oracle.tls.skip_verify`   | `false`   | `true` → skip server certificate verification.                        |
| `adbc.oracle.tls.server_name`   | (host)    | Host name for certificate verification / SNI.                         |
| `adbc.oracle.wallet_location`   | (none)    | Directory containing `ewallet.pem` (Autonomous Database wallet); implies TLS. mTLS works with an unencrypted key. |
| `adbc.oracle.token`             | (none)    | OAuth / IAM bearer token instead of a password (TLS only).            |
| `adbc.oracle.mode`              | (none)    | `sysdba`, `sysoper`, `sysasm`, `sysbackup`, `sysdg`, `syskm`, `sysrac`. |
| `adbc.oracle.connect_timeout`   | `30`      | Dial timeout, as seconds or a Go duration like `1.5s`.                |
| `adbc.oracle.batch_size`        | `65536`   | Maximum rows per Arrow record batch.                                  |
| `adbc.oracle.prefetch_rows`     | (batch size, max 65536) | Rows the server returns per fetch round trip.           |
| `adbc.oracle.number_mode`       | `auto`    | `NUMBER` → Arrow policy: `auto`, `decimal`, `double`, `string` (see [Type mapping](#type-mapping)). |
| `adbc.oracle.interval_mode`     | `monthdaynano` | `INTERVAL` → Arrow policy: `monthdaynano`, `duration` (DAY TO SECOND), `string` (ISO 8601). |
| `adbc.oracle.date_mode`         | `timestamp` | `DATE` → `timestamp[s]` (keeps the time of day) or `date32`.        |
| `adbc.oracle.batch_bytes`       | `0`       | Approximate upper bound on bytes per Arrow record batch (0 = only `batch_size` applies). |
| `adbc.oracle.disable_oob`       | `false`   | Disable out-of-band (TCP urgent) breaks used for cancellation; falls back to in-band markers. |
| `adbc.oracle.nne`               | `accepted` | Native Network Encryption / data integrity: `accepted`, `requested`, `required`, `rejected`. |
| `adbc.oracle.nne_checksum`      | (=`nne`)  | Data-integrity level, if different from `nne`.                        |
| `adbc.oracle.nne_encryption_algorithms` | (all AES) | Comma-separated encryption preference, e.g. `AES256,AES192`. |
| `adbc.oracle.nne_checksum_algorithms`   | (all)     | Comma-separated checksum preference, e.g. `SHA512,SHA256`. |
| `adbc.oracle.use_extension_types` | `false` | Annotate JSON / object (`arrow.json`), XMLType (`arrow.opaque`) and SDO_GEOMETRY (`geoarrow.wkb`) columns with Arrow extension-type metadata. |
| `adbc.oracle.session_time_zone` | `+00:00`  | Session `TIME_ZONE`; `TIMESTAMP WITH LOCAL TIME ZONE` values are returned in it. |
| `adbc.oracle.sdu`               | (server)  | Requested session data unit (packet size) in bytes.                   |
| `adbc.oracle.application_name`  | (executable name) | Program name reported to the server (`V$SESSION.PROGRAM`, `CLIENT_PROGRAM_NAME`). |
| `adbc.oracle.current_schema`    | (none)    | Sets the session's current schema after connecting.                   |
| `adbc.oracle.trace`             | `false`   | `true` → hex-dump TNS packets to stderr.                              |

All of these are also accepted as `?key=value` URI query parameters
(without the `adbc.oracle.` prefix, e.g. `?tls=true&number_mode=decimal`).
After connecting, `adbc.oracle.batch_size`, `adbc.oracle.batch_bytes`,
`adbc.oracle.prefetch_rows`, `adbc.oracle.number_mode`,
`adbc.oracle.interval_mode`, `adbc.oracle.date_mode` and the end-to-end tracing attributes
`adbc.oracle.module` / `.action` / `.client_info` / `.client_identifier`
can be changed per connection.

The URI is its own kwarg; everything else goes through `db_kwargs`:

```python
import adbc_driver_oracle.dbapi as oracle

oracle.connect(
    uri="oracle://db.example.com:2484/PROD",
    db_kwargs={
        "username": "app",
        "password": "s3cret",
        "adbc.oracle.tls": "true",
        "adbc.oracle.tls.ca_cert": "/etc/ssl/certs/corp-ca.pem",
    },
)
```

## Connection profiles & driver manifests

[ADBC connection profiles](https://arrow.apache.org/adbc/current/format/connection_profiles.html)
(adbc-driver-manager ≥ 1.11) let you keep a connection's driver +
options in a reusable TOML file instead of code. Profiles resolve the
driver by *name*, which requires a
[driver manifest](https://arrow.apache.org/adbc/current/format/driver_manifests.html)
on the search path. Install ours once per environment:

```console
$ python -m adbc_driver_oracle install-manifest
Wrote ADBC driver manifest: .../etc/adbc/drivers/oracle.toml
```

(Inside a virtualenv/conda env this targets the environment's
auto-searched `etc/adbc/drivers/`; otherwise the per-user ADBC config
directory. `--user`, `--venv`, and `--dir PATH` override; the same is
available programmatically as `adbc_driver_oracle.install_manifest()`.)

With the manifest in place, the driver manager finds the driver by name
— no import of `adbc_driver_oracle` needed:

```python
from adbc_driver_manager import dbapi

# Resolve by URI scheme alone:
conn = dbapi.connect(uri="oracle://scott:tiger@localhost:1521/FREEPDB1")
```

And a profile bundles the whole connection. Drop this in
`~/.config/adbc/profiles/oracle_prod.toml` (Linux;
`~/Library/Application Support/ADBC/Profiles/` on macOS, or any
directory named in `ADBC_PROFILE_PATH`):

```toml
profile_version = 1
driver = "oracle"

[Options]
uri = "oracle://db.example.com:2484/PROD"
username = "app"
password = "{{ env_var(ORACLE_PASSWORD) }}"
"adbc.oracle.tls" = true
```

then connect from any ADBC driver-manager binding:

```python
conn = dbapi.connect(profile="oracle_prod")
```

The `{{ env_var(...) }}` substitution keeps secrets out of the file;
options set explicitly in code still override profile values.

## Using from Go

```go
import (
    "context"

    "github.com/apache/arrow-go/v18/arrow/memory"
    "github.com/gizmodata/adbc-driver-oracle/driver/oracle"
)

drv := oracle.NewDriver(memory.DefaultAllocator)
db, _ := drv.NewDatabase(map[string]string{
    "uri": "oracle://scott:tiger@localhost:1521/FREEPDB1",
})
conn, _ := db.Open(context.Background())
stmt, _ := conn.NewStatement()
_ = stmt.SetSqlQuery("SELECT ename, sal FROM emp")
reader, _, _ := stmt.ExecuteQuery(context.Background())
defer reader.Release()
for reader.Next() {
    rec := reader.Record()
    // ...
}
```

## Type mapping

Reads (`adbc.oracle.number_mode=auto`, the default):

| Oracle type                              | Arrow type                                   |
|------------------------------------------|----------------------------------------------|
| `NUMBER(p,0)` with 1 ≤ p ≤ 18            | `int64`                                      |
| `NUMBER(p,s)` with 1 ≤ p ≤ 38            | `decimal128(p,s)`                            |
| `NUMBER` (no precision), `FLOAT`, computed expressions (`COUNT(*)`, `1/3`, literals) | `float64` |
| `BINARY_FLOAT` / `BINARY_DOUBLE`         | `float32` / `float64`                        |
| `CHAR`, `VARCHAR2`, `NCHAR`, `NVARCHAR2`, `LONG`, `CLOB`, `NCLOB` | `utf8`               |
| `RAW`, `LONG RAW`, `BLOB`                | `binary`                                     |
| `DATE`                                   | `timestamp[s]` (`date32` with `date_mode=date32`) |
| `TIMESTAMP(n)`                           | `timestamp[s / ms / us / ns]` by fractional-second precision n |
| `TIMESTAMP WITH TIME ZONE` / `WITH LOCAL TIME ZONE` | `timestamp[…, tz=UTC]` (the instant; the original offset is not kept) |
| `INTERVAL DAY TO SECOND`                 | `month_day_nano_interval`; `duration[unit by precision]` with `interval_mode=duration`; ISO-8601 `utf8` (`P1DT2H3M4.5S`) with `interval_mode=string` |
| `INTERVAL YEAR TO MONTH`                 | `month_day_nano_interval`; ISO-8601 `utf8` (`P2Y3M`) with `interval_mode=string` |
| `ROWID` / `UROWID`                       | `utf8`                                       |
| `JSON` (21c+)                            | `utf8` — native OSON decoded to JSON text client-side |
| `BOOLEAN` (23ai)                         | `bool`                                       |
| Object types, VARRAY, nested table       | `utf8` JSON text (`arrow.json` extension with `use_extension_types`) |
| `XMLTYPE`                                | `utf8` XML text (`arrow.opaque` extension)   |
| `MDSYS.SDO_GEOMETRY`                     | `binary` WKB (`geoarrow.wkb` extension)      |

`number_mode=decimal` maps every `NUMBER` to `decimal128` (`(38,10)` when
the precision is unknown), `double` maps all of them to `float64`, and
`string` returns the exact decimal text — useful when precision matters
and a column's declared scale can't be trusted.

Writes (bind parameters and bulk-ingest DDL):

| Arrow type                       | Bind type          | Generated DDL                          |
|----------------------------------|--------------------|----------------------------------------|
| `int8/16/32/64`, `uint*`         | `NUMBER`           | `NUMBER(3/5/10/19/20)`                 |
| `float16` / `float32` / `float64` | `BINARY_FLOAT` / `BINARY_FLOAT` / `BINARY_DOUBLE` | same         |
| `decimal128/256(p,s)`            | `NUMBER`           | `NUMBER(p,s)`                          |
| `utf8`, `large_utf8`, `utf8_view` | `VARCHAR2` (`LONG` above the server max) | `VARCHAR2(4000)` (`CLOB` for `large_utf8`; see ingest options) |
| `binary`, `fixed_size_binary`, `large_binary` | `RAW` (`LONG RAW` above the max) | `RAW(2000)` / `RAW(n)` / `BLOB` |
| `bool`                           | `BOOLEAN` on 23ai, else `NUMBER` 0/1 | `BOOLEAN` / `NUMBER(1)`  |
| `date32` / `date64`              | `DATE`             | `DATE`                                 |
| `timestamp[unit]` (naive)        | `TIMESTAMP`        | `TIMESTAMP(0/3/6/9)`                   |
| `timestamp[unit, tz]`            | `TIMESTAMP WITH TIME ZONE` (as UTC) | `TIMESTAMP(n) WITH TIME ZONE` |
| `duration`, `month_day_nano_interval` (no months) | `INTERVAL DAY TO SECOND` | `INTERVAL DAY(9) TO SECOND(9)` |
| `list`, `struct`, `map`          | `VARCHAR2` / `LONG` (JSON text) | `JSON` (21c+) — see `ingest.struct_type` |

Empty strings are bound as NULL, matching Oracle's own `''` semantics.

## Limitations

- **Kerberos / RADIUS** network authentication is not supported (use
  password, TLS, wallet or token auth). Native Network Encryption *is*
  supported (see [above](#native-network-encryption-nne)).
- `REF CURSOR` bind/column values, `BFILE`, LOB-typed `OUT` binds,
  `VECTOR`, Advanced Queuing, objects stored in LOBs (degenerate images)
  and SDO_GEOMETRY circular arcs / compound elements are not supported
  yet (select `VECTOR_SERIALIZE(...)`, `SDO_UTIL.TO_WKBGEOMETRY(...)` etc.
  to read those as text/binary). Object types are read-only: they cannot
  be bound as parameters or written by ingest.
- Strings inside object images are decoded as UTF-8 (AL32UTF8 databases;
  national-character attributes as UTF-16).
- Named time-zone regions in `TIMESTAMP WITH TIME ZONE` values are
  returned as UTC instants (offset-based zones are exact).
- Kerberos / RADIUS / external OS authentication, DRCP pooling and
  Oracle wallet private keys with a password are not supported.
- Cancellation is only honoured by the server at SQL execution
  checkpoints; a call blocked in e.g. `DBMS_SESSION.SLEEP` finishes
  before `ORA-01013` is raised (python-oracledb behaves the same).

## Repo layout

```
adbc-driver-oracle/
├── go.mod, go.sum
├── internal/
│   ├── tns/         — TNS packets (CONNECT/ACCEPT/REDIRECT/DATA/MARKER), TLS, SDU, Native Network Encryption (ANO)
│   ├── ttc/         — TTC message layer: protocol/data-type negotiation, O5LOGON auth, cursors, fetch
│   └── oratype/     — NUMBER / DATE / TIMESTAMP / INTERVAL / ROWID / OSON (JSON) codecs
├── driver/oracle/   — pure-Go ADBC Driver/Database/Connection/Statement impl
├── pkg/oracle/      — cgo c-shared wrapper (produces libadbc_driver_oracle.{so,dylib,dll})
├── python/          — Python wheel sources (adbc_driver_oracle)
└── .github/         — CI: go test, python tests (Oracle Free + GizmoSQL services), wheel matrix, PyPI publish
```

## Provenance & licensing

This is an **independent, from-scratch reimplementation** of a client for
Oracle Database's TNS/TTC network protocol, written in Go. It contains **no
Oracle Corporation source code** and links against **no Oracle Corporation
libraries** — that's the whole point (no Instant Client). Network protocols
and the interfaces needed for interoperability are not, in themselves,
proprietary to any vendor.

The wire behavior was implemented by reference to **publicly available,
openly licensed** source — no confidential specification, non-public
documentation, or binary disassembly was used:

- [`oracle/python-oracledb`](https://github.com/oracle/python-oracledb) —
  Oracle's *own* open-source driver, whose "thin mode" speaks the same
  protocol, published by Oracle under a dual **UPL 1.0 / Apache-2.0** license.
  It is the authoritative public reference for the protocol.
- [`sijms/go-ora`](https://github.com/sijms/go-ora) — a community pure-Go
  Oracle client (**MIT**); the Native Network Encryption negotiation was
  reimplemented by reference to it.
- [Apache Arrow ADBC](https://github.com/apache/arrow-adbc) and
  [Apache Arrow Go](https://github.com/apache/arrow-go) (**Apache-2.0**) —
  the ADBC framework and Arrow libraries.

See [`NOTICE`](NOTICE) for full attribution.

## Trademarks

Oracle and Oracle Database are registered trademarks of Oracle Corporation.
**adbc-driver-oracle is not affiliated with, endorsed by, or sponsored by
Oracle Corporation.** References to "Oracle Database" identify the software
this driver interoperates with and are nominative fair use.

## License

[MIT](https://github.com/gizmodata/adbc-driver-oracle/blob/main/LICENSE) —
Copyright (c) 2026 GizmoData LLC. See [`NOTICE`](NOTICE) for third-party
attributions.
