# adbc-driver-oracle - by [GizmoData™](https://gizmodata.com)

[![GitHub](https://img.shields.io/badge/GitHub-gizmodata%2Fadbc--driver--oracle-blue.svg?logo=Github)](https://github.com/gizmodata/adbc-driver-oracle)
[![CI](https://github.com/gizmodata/adbc-driver-oracle/actions/workflows/ci.yml/badge.svg)](https://github.com/gizmodata/adbc-driver-oracle/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/gizmodata/adbc-driver-oracle.svg)](https://pkg.go.dev/github.com/gizmodata/adbc-driver-oracle)
[![Go Version](https://img.shields.io/github/go-mod/go-version/gizmodata/adbc-driver-oracle)](https://github.com/gizmodata/adbc-driver-oracle/blob/main/go.mod)
[![Supported Python Versions](https://img.shields.io/pypi/pyversions/adbc-driver-oracle)](https://pypi.org/project/adbc-driver-oracle/)
[![PyPI version](https://img.shields.io/pypi/v/adbc-driver-oracle?logo=pypi)](https://pypi.org/project/adbc-driver-oracle/)
[![PyPI Downloads](https://img.shields.io/pepy/dt/adbc-driver-oracle.svg)](https://pepy.tech/projects/adbc-driver-oracle)
[![ADBC conformance](https://img.shields.io/badge/ADBC-conformance%20suite%20passing-2ea44f.svg)](https://github.com/gizmodata/adbc-driver-oracle/blob/main/validation/README.md)
[![Follows the ADBC driver style guide](https://img.shields.io/badge/ADBC%20style%20guide-conforms-2ea44f.svg)](https://adbc-drivers.org/building-drivers/style-guide.html)
[![License](https://img.shields.io/github/license/gizmodata/adbc-driver-oracle)](https://github.com/gizmodata/adbc-driver-oracle/blob/main/LICENSE)
[![Hardcharger](https://img.shields.io/badge/works%20with-Hardcharger%E2%84%A2-f15a24.svg)](https://gizmodata.com/hardcharger)

<h3 align="center">Arrow straight from Oracle Database, in Python: pure Go, no Oracle Client libraries required.</h3>

---

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

## 📦 Install

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

## 🔌 Connect and query

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

## 🌊 Stream large results

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

## ⚙️ Options

Driver options go in `db_kwargs` and follow the
[ADBC driver style guide](https://adbc-drivers.org/building-drivers/style-guide.html):
`oracle.<option>`, grouped as `oracle.query.*` (result batches and Arrow types),
`oracle.ingest.*` (bulk ingest), `oracle.tls.*`, `oracle.auth.*`, `oracle.session.*` and
`oracle.nne.*` (Native Network Encryption). The `adbc_driver_oracle.DatabaseOptions`,
`ConnectionOptions` and `StatementOptions` enums list them:

```python
import adbc_driver_oracle.dbapi as oracle

with oracle.connect(
    uri="oracle://scott:tiger@localhost:1521/FREEPDB1",
    db_kwargs={"oracle.query.batch_size": "10000", "oracle.query.number_type": "decimal"},
) as conn, conn.cursor() as cur:
    cur.execute("SELECT 1.5 AS x FROM DUAL")
    print(cur.fetch_arrow_table().schema)
```

Names before 1.1.0 (`adbc.oracle.*`) still work and print a one-time deprecation warning to
stderr naming the new key; they will be removed in 2.0.0. The
[project README](https://github.com/gizmodata/adbc-driver-oracle#connection-url) has every option.

## 🧭 Load the driver by name (driver manifest)

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

## 📚 More

- [Project README](https://github.com/gizmodata/adbc-driver-oracle#readme): every option and
  feature, other languages, and the one-line installer for the shared library
- [Changelog](https://github.com/gizmodata/adbc-driver-oracle/blob/main/CHANGELOG.md)
- [Issues](https://github.com/gizmodata/adbc-driver-oracle/issues)

## ™️ Trademarks

Oracle and Oracle Database are registered trademarks of Oracle Corporation.
**adbc-driver-oracle is not affiliated with, endorsed by, or sponsored by
Oracle Corporation.** References to "Oracle Database" identify the software
this driver interoperates with and are nominative fair use.

## 📄 License

[MIT](https://github.com/gizmodata/adbc-driver-oracle/blob/main/LICENSE) —
Copyright (c) 2026 GizmoData LLC. See
[`NOTICE`](https://github.com/gizmodata/adbc-driver-oracle/blob/main/NOTICE) for third-party
attributions.

---

<p align="center">
  <a href="https://gizmodata.com"><img src="https://raw.githubusercontent.com/gizmodata/adbc-driver-oracle/main/docs/assets/gizmodata-logo.png" alt="GizmoData™" width="56"></a><br>
  <sub>Built by <a href="https://gizmodata.com">GizmoData™</a>, makers of <a href="https://gizmodata.com/hardcharger">Hardcharger™</a>. Free and open source.</sub>
</p>

<p align="center"><sub>Hardcharger™ and GizmoData™ are trademarks of GizmoData LLC.</sub></p>
