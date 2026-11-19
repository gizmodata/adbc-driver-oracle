# adbc-driver-oracle

**Python package for [adbc-driver-oracle](https://github.com/gizmodata/adbc-driver-oracle), an
[Apache Arrow ADBC](https://arrow.apache.org/adbc/) driver for Oracle Database, by
[GizmoData™](https://gizmodata.com): pure Go, no Oracle Client libraries required.**

It speaks Oracle's native TNS/TTC wire protocol and returns Apache Arrow data straight from the
result set. No Instant Client, no `ORACLE_HOME`, no `LD_LIBRARY_PATH`: the driver is a shared
library inside the wheel.

> An independent, open-source project. Not affiliated with, endorsed by, or sponsored by Oracle
> Corporation. "Oracle" and "Oracle Database" are trademarks of Oracle Corporation, used here only
> to describe compatibility.

This page covers the Python package. For other languages (C, C++, Go, Rust, R, DuckDB), the
one-line installer of the shared library (`curl https://install.adbc.gizmodata.com/oracle | sh`),
the connection URL reference, type mapping, bulk ingest, PL/SQL, Native Network Encryption and
limitations, see the [project README on GitHub](https://github.com/gizmodata/adbc-driver-oracle#readme).

## Install

```bash
pip install adbc-driver-oracle
```

- Python 3.10 or later.
- Wheels for Linux x86_64 and aarch64 (glibc 2.17 or later), macOS arm64 (13 or later) and
  Windows x64.
- Depends on `adbc-driver-manager` and `pyarrow`.
- Oracle Database 12.1 or later (tested against Oracle Database 23ai Free).

For a local database, the [gvenzl/oracle-free](https://github.com/gvenzl/oci-oracle-free)
container gives you `oracle://scott:tiger@localhost:1521/FREEPDB1`:

```bash
docker run --name oracle-adbc-test -d -p 1521:1521 \
  -e ORACLE_PASSWORD=tiger -e APP_USER=scott -e APP_USER_PASSWORD=tiger \
  gvenzl/oracle-free:23-slim-faststart
```

## Connect and query

```python
import adbc_driver_oracle.dbapi as oracle

with oracle.connect(
    uri="oracle://scott:tiger@localhost:1521/FREEPDB1",
) as conn, conn.cursor() as cur:
    cur.execute("SELECT 42 AS answer, 'hello oracle' AS greeting FROM DUAL")
    table = cur.fetch_arrow_table()  # a pyarrow.Table
    print(table)
```

The result is a `pyarrow.Table`: pass it to Polars, pandas, DuckDB or anything else that reads
Arrow.

Credentials can be options instead of part of the URL:

```python
import adbc_driver_oracle.dbapi as oracle

with oracle.connect(
    uri="oracle://localhost:1521/FREEPDB1",
    db_kwargs={"username": "scott", "password": "tiger"},
) as conn, conn.cursor() as cur:
    cur.execute("SELECT USER FROM DUAL")
    print(cur.fetchone())
```

## Stream large results

`fetch_record_batch()` returns a `pyarrow.RecordBatchReader` that pulls rows from the server one
batch at a time, so memory stays bounded however large the result is:

```python
import adbc_driver_oracle.dbapi as oracle

with oracle.connect(
    uri="oracle://scott:tiger@localhost:1521/FREEPDB1",
) as conn, conn.cursor() as cur:
    cur.execute("SELECT LEVEL AS n FROM DUAL CONNECT BY LEVEL <= 100000")
    rows = 0
    for batch in cur.fetch_record_batch():
        rows += batch.num_rows
    print(rows)
```

## Load the driver by name (driver manifest)

ADBC [driver manifests](https://arrow.apache.org/adbc/current/format/driver_manifests.html) let
the ADBC driver manager, and [connection profiles](https://arrow.apache.org/adbc/current/format/connection_profiles.html),
find a driver by name. The package writes one for its own library:

```console
$ python -m adbc_driver_oracle install-manifest
Wrote ADBC driver manifest: .../etc/adbc/drivers/oracle.toml
```

(Inside a virtualenv or conda environment this targets the environment's `etc/adbc/drivers/`;
otherwise the per-user ADBC directory. `--user`, `--venv` and `--dir PATH` override it, and
`adbc_driver_oracle.install_manifest()` does the same from Python.) Then the driver manager
resolves `oracle://` URLs with no import of `adbc_driver_oracle`:

```python
from adbc_driver_manager import dbapi

with dbapi.connect(uri="oracle://scott:tiger@localhost:1521/FREEPDB1") as conn, conn.cursor() as cur:
    cur.execute("SELECT 1 AS one FROM DUAL")
    print(cur.fetch_arrow_table())
```

## More

- [Project README](https://github.com/gizmodata/adbc-driver-oracle#readme): every option and
  feature, other languages, and the one-line installer for the shared library
- [Changelog](https://github.com/gizmodata/adbc-driver-oracle/blob/main/CHANGELOG.md)
- [Issues](https://github.com/gizmodata/adbc-driver-oracle/issues)

Licensed under the [MIT License](https://github.com/gizmodata/adbc-driver-oracle/blob/main/LICENSE).
