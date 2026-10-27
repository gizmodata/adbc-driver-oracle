"""Tier-3 integration test: Hardcharger (https://gizmodata.com/hardcharger)
and embedded DuckDB reading from and writing to Oracle *live* through the
``adbc_scanner`` extension, loading this repo's c-shared driver library:

    Hardcharger / duckdb ──adbc_scanner──▶ libadbc_driver_oracle ──TNS/TTC──▶ Oracle

Every test runs twice: inside a Hardcharger server (the SQL goes over Warp SQL
and the server loads the driver) and in an in-process DuckDB. Both directions
are exercised: ``adbc_scan`` pulls Oracle result sets in, and writes go back to
Oracle two ways — ``CREATE TABLE ... AS`` through the attached catalog
(``USE ora.<schema>``) and the ``adbc_insert`` function.

On DuckDB 2.0 and later (every Hardcharger) the extension comes from the
Query.Farm repository; DuckDB 1.x installs it from community.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass

import pytest

from .conftest import drop_table_quietly

pytestmark = [pytest.mark.integration, pytest.mark.duckdb_ext]


def lit(value: str) -> str:
    """A SQL string literal (the SQL travels as text, so no bind parameters)."""
    return "'" + value.replace("'", "''") + "'"


@dataclass
class Engine:
    """Somewhere to run DuckDB SQL: an in-process DuckDB or a Hardcharger session."""

    name: str
    run: Callable[[str], list[tuple]]
    catalog: str  # the engine's own default catalog, for USE after a write into Oracle

    def one(self, sql: str) -> tuple:
        (row,) = self.run(sql)
        return row


def install_adbc_scanner(run: Callable[[str], list[tuple]]) -> None:
    ((version,),) = run("SELECT version()")
    if int(version.lstrip("v").split(".")[0]) >= 2:
        run("SET allow_extension_repositories = 'allowed'")
        run(
            "CREATE EXTENSION REPOSITORY IF NOT EXISTS query_farm "
            "WITH PREFIX 'https://duckdb-extensions.query.farm'"
        )
        run("INSTALL adbc_scanner FROM query_farm")
        run("LOAD adbc_scanner FROM query_farm")
    else:
        run("INSTALL adbc_scanner FROM community")
        run("LOAD adbc_scanner")


@pytest.fixture(
    params=[
        pytest.param("hardcharger", marks=pytest.mark.hardcharger),
        pytest.param("duckdb"),
    ]
)
def engine(request, oracle_server, driver_path):
    if request.param == "hardcharger":
        server = request.getfixturevalue("hardcharger_server")
        conn = server.connect(autocommit=True)
        cur = conn.cursor()

        def run(sql: str) -> list[tuple]:
            cur.execute(operation=sql)
            return cur.fetchall() if cur.description else []

        close = conn.close
        library = server.driver_path or driver_path
    else:
        duckdb = pytest.importorskip("duckdb")
        con = duckdb.connect(database=":memory:")

        def run(sql: str) -> list[tuple]:
            return con.execute(query=sql).fetchall()

        close = con.close
        library = driver_path

    try:
        install_adbc_scanner(run=run)
        ((catalog,),) = run("SELECT current_database()")
        # Credentials live in a secret (README "Hardcharger / DuckDB <-> Oracle via
        # adbc_scanner"); ATTACH exposes Oracle as a catalog, and the function API
        # (adbc_scan / adbc_insert) takes the attached database's name.
        uri = oracle_server.credential_free_uri
        run(
            f"""CREATE OR REPLACE SECRET ora_secret (
                TYPE adbc,
                SCOPE {lit(uri)},
                driver {lit(library)},
                entrypoint 'OracleDriverInit',
                uri {lit(uri)},
                username {lit(oracle_server.username)},
                password {lit(oracle_server.password)}
            )"""
        )
        run("DETACH DATABASE IF EXISTS ora")
        run(f"ATTACH {lit(uri)} AS ora (TYPE adbc)")
    except Exception:
        close()
        raise
    yield Engine(name=request.param, run=run, catalog=catalog)
    try:
        run(f'USE "{catalog}"')
        run("DETACH ora")
        run("DROP SECRET ora_secret")
    finally:
        close()


def test_adbc_scan_pulls_from_oracle(engine):
    # Pull: an Oracle result set materialised in the engine.
    n, first_owner = engine.one(
        """SELECT COUNT(*) AS n, MIN(OWNER) AS first_owner
           FROM adbc_scan('ora', 'SELECT OWNER, OBJECT_NAME FROM ALL_OBJECTS WHERE ROWNUM <= 5000')"""
    )
    assert n > 100
    assert first_owner

    # Types survive the trip: Oracle NUMBER(p,s)/TIMESTAMP/VARCHAR2 become
    # DuckDB DECIMAL/TIMESTAMP/VARCHAR.
    typed = engine.run(
        """DESCRIBE SELECT * FROM adbc_scan('ora',
           'SELECT CAST(1.25 AS NUMBER(10,2)) AS D, CAST(7 AS NUMBER(10)) AS I, SYSTIMESTAMP AS TS, ''x'' AS S FROM DUAL')"""
    )
    kinds = {name: typ for name, typ, *_ in typed}
    assert kinds["D"].startswith("DECIMAL")
    assert kinds["I"] == "BIGINT"
    assert kinds["TS"].startswith("TIMESTAMP")
    assert kinds["S"] == "VARCHAR"


def test_attach_ctas_writes_to_oracle(engine, oracle_server, table_name):
    """Push via the attached catalog with plain SQL — no adbc_insert().

    ``USE ora.<schema>`` then ``CREATE TABLE ... AS SELECT`` bulk-ingests a
    relation straight into Oracle through adbc_scanner's ATTACH, i.e. parity
    with the adbc_insert() function for the common create-and-load case.
    """
    import adbc_driver_oracle.dbapi as oracle

    rows = 5_000
    local = f"local_{table_name.lower()}"  # a Hardcharger server is shared: no fixed names
    engine.run(
        f"""CREATE TABLE "{engine.catalog}".main.{local} AS
            SELECT i AS id, 'row-' || i AS name FROM range(1, {rows + 1}) t(i)"""
    )
    schema = oracle_server.username.upper()
    try:
        # Write through the attached catalog: switch into the Oracle schema,
        # then CREATE TABLE ... AS. No function call, just SQL.
        engine.run(f'USE ora."{schema}"')
        engine.run(f'CREATE TABLE "{table_name}" AS SELECT * FROM "{engine.catalog}".main.{local}')
        engine.run(f'USE "{engine.catalog}"')

        # Read it back through the attached catalog (projection + filter pushdown).
        (n_attached,) = engine.one(
            f'SELECT COUNT(*) FROM ora."{schema}"."{table_name}" WHERE ID > {rows - 100}'
        )
        assert n_attached == 100

        # ... and independently through the plain Python DBAPI.
        with oracle.connect(uri=oracle_server.uri) as conn, conn.cursor() as cur:
            cur.execute(operation=f"SELECT COUNT(*), SUM(id) FROM {table_name}")
            count, total = cur.fetchone()
            assert count == rows
            assert total == rows * (rows + 1) // 2
    finally:
        engine.run(f'USE "{engine.catalog}"')
        engine.run(f'DROP TABLE IF EXISTS "{engine.catalog}".main.{local}')
        with oracle.connect(uri=oracle_server.uri) as conn, conn.cursor() as cur:
            drop_table_quietly(cur, table_name)


def test_adbc_insert_pushes_to_oracle(engine, oracle_server, table_name):
    import adbc_driver_oracle.dbapi as oracle

    rows = 10_000
    local = f"local_{table_name.lower()}"
    engine.run(
        f"""CREATE TABLE "{engine.catalog}".main.{local} AS
            SELECT i AS id, 'row-' || i AS name, (i * 0.5)::DECIMAL(12,2) AS amt,
                   TIMESTAMP '2024-01-02 03:04:05' + INTERVAL (i) SECOND AS ts
            FROM range(1, {rows + 1}) t(i)"""
    )
    try:
        # Push: a relation lands in a brand-new Oracle table.
        (inserted,) = engine.one(
            f"""SELECT * FROM adbc_insert('ora', {lit(table_name)},
                    (SELECT * FROM "{engine.catalog}".main.{local}), mode := 'create')"""
        )
        assert inserted == rows

        # Round trip: read it back through adbc_scan ...
        n, total, max_name = engine.one(
            f"""SELECT COUNT(*), SUM(ID), MAX(NAME)
                FROM adbc_scan('ora', {lit(f"SELECT id, name FROM {table_name}")})"""
        )
        assert n == rows
        assert total == rows * (rows + 1) // 2
        assert max_name == "row-9999"

        # ... through the attached catalog (projection + filter pushdown) ...
        schema = oracle_server.username.upper()
        (n_attached,) = engine.one(
            f'SELECT COUNT(*) FROM ora."{schema}"."{table_name}" WHERE ID > {rows - 100}'
        )
        assert n_attached == 100

        # ... and independently through the plain Python DBAPI.
        with oracle.connect(uri=oracle_server.uri) as conn, conn.cursor() as cur:
            cur.execute(operation=f"SELECT COUNT(*), MIN(ts), SUM(amt) FROM {table_name}")
            count, min_ts, amt_sum = cur.fetchone()
            assert count == rows
            assert str(min_ts).startswith("2024-01-02 03:04:06")
            assert float(amt_sum) == rows * (rows + 1) // 2 * 0.5
    finally:
        engine.run(f'DROP TABLE IF EXISTS "{engine.catalog}".main.{local}')
        with oracle.connect(uri=oracle_server.uri) as conn, conn.cursor() as cur:
            drop_table_quietly(cur, table_name)
