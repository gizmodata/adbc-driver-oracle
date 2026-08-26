"""Tier-3 integration test: DuckDB (and therefore GizmoSQL, which embeds
DuckDB) reading from and writing to Oracle *live* through the
``adbc_scanner`` community extension, loading this repo's c-shared driver
library:

    duckdb ──adbc_scanner──▶ libadbc_driver_oracle ──TNS/TTC──▶ Oracle

Both directions are exercised: ``adbc_scan`` pulls Oracle result sets into
DuckDB, ``adbc_insert`` pushes a DuckDB relation into a new Oracle table.

Skipped when the ``duckdb`` package or the extension is unavailable.
"""

from __future__ import annotations

import pytest

from .conftest import drop_table_quietly

pytestmark = [pytest.mark.integration, pytest.mark.duckdb_ext]


@pytest.fixture()
def duckdb_oracle(oracle_server):
    duckdb = pytest.importorskip("duckdb")
    import adbc_driver_oracle

    con = duckdb.connect(database=":memory:")
    try:
        con.execute(query="INSTALL adbc_scanner FROM community")
        con.execute(query="LOAD adbc_scanner")
    except Exception as exc:  # pragma: no cover - environment dependent
        con.close()
        pytest.skip(reason=f"adbc_scanner extension unavailable: {exc}")

    con.execute(
        query="""SET VARIABLE ora = adbc_connect({
            'driver': $driver,
            'entrypoint': 'OracleDriverInit',
            'uri': $uri,
            'username': $username,
            'password': $password
        })""",
        parameters={
            "driver": adbc_driver_oracle._driver_path(),
            "uri": oracle_server.credential_free_uri,
            "username": oracle_server.username,
            "password": oracle_server.password,
        },
    )
    yield con
    con.execute(query="SELECT adbc_disconnect(getvariable('ora')::BIGINT)")
    con.close()


def test_duckdb_adbc_scan_pulls_from_oracle(duckdb_oracle):
    con = duckdb_oracle

    # Pull: an Oracle result set materialised inside DuckDB.
    n, first_owner = con.execute(
        query="""SELECT COUNT(*) AS n, MIN(OWNER) AS first_owner
                 FROM adbc_scan(getvariable('ora')::BIGINT,
                                'SELECT OWNER, OBJECT_NAME FROM ALL_OBJECTS WHERE ROWNUM <= 5000')"""
    ).fetchone()
    assert n > 100
    assert first_owner

    # Types survive the trip: Oracle NUMBER(p,s)/TIMESTAMP/VARCHAR2 become
    # DuckDB DECIMAL/TIMESTAMP/VARCHAR.
    typed = con.execute(
        query="""DESCRIBE SELECT * FROM adbc_scan(getvariable('ora')::BIGINT,
                 'SELECT CAST(1.25 AS NUMBER(10,2)) AS D, CAST(7 AS NUMBER(10)) AS I, SYSTIMESTAMP AS TS, ''x'' AS S FROM DUAL')"""
    ).fetchall()
    kinds = {name: typ for name, typ, *_ in typed}
    assert kinds["D"].startswith("DECIMAL")
    assert kinds["I"] == "BIGINT"
    assert kinds["TS"].startswith("TIMESTAMP")
    assert kinds["S"] == "VARCHAR"


def test_duckdb_adbc_insert_pushes_to_oracle(duckdb_oracle, oracle_server, table_name):
    con = duckdb_oracle
    import adbc_driver_oracle.dbapi as oracle

    rows = 10_000
    con.execute(
        query=f"""CREATE TABLE local_src AS
                  SELECT i AS id, 'row-' || i AS name, (i * 0.5)::DECIMAL(12,2) AS amt,
                         TIMESTAMP '2024-01-02 03:04:05' + INTERVAL (i) SECOND AS ts
                  FROM range(1, {rows + 1}) t(i)"""
    )

    # Push: a DuckDB relation lands in a brand-new Oracle table.
    (inserted,) = con.execute(
        query="""SELECT * FROM adbc_insert(getvariable('ora')::BIGINT, $table,
                     (SELECT * FROM local_src), mode := 'create')""",
        parameters={"table": table_name},
    ).fetchone()
    assert inserted == rows

    try:
        # Round trip: read it back through adbc_scan ...
        (n, total, max_name) = con.execute(
            query="""SELECT COUNT(*), SUM(ID), MAX(NAME)
                     FROM adbc_scan(getvariable('ora')::BIGINT, $sql)""",
            parameters={"sql": f"SELECT id, name FROM {table_name}"},
        ).fetchone()
        assert n == rows
        assert total == rows * (rows + 1) // 2
        assert max_name == "row-9999"

        # ... and independently through the plain Python DBAPI.
        with oracle.connect(uri=oracle_server.uri) as conn, conn.cursor() as cur:
            cur.execute(f"SELECT COUNT(*), MIN(ts), SUM(amt) FROM {table_name}")
            count, min_ts, amt_sum = cur.fetchone()
            assert count == rows
            assert str(min_ts).startswith("2024-01-02 03:04:06")
            assert float(amt_sum) == rows * (rows + 1) // 2 * 0.5
    finally:
        with oracle.connect(uri=oracle_server.uri) as conn, conn.cursor() as cur:
            drop_table_quietly(cur, table_name)
