"""End-to-end ADBC tests against a live Oracle Database.

Several tests below mirror snippets in the project README — see each
test's docstring for the section it covers. Keeping these in lockstep
matters: if the README claims a pattern works, a test in this file
must prove it does.
"""

from __future__ import annotations

import datetime
import decimal
import json

import pyarrow as pa
import pytest

from .conftest import drop_table_quietly

pytestmark = pytest.mark.integration


def _connect(server, *, autocommit: bool = True):
    import adbc_driver_oracle.dbapi as oracle

    return oracle.connect(uri=server.uri, autocommit=autocommit)


def _rows(reader) -> list[dict]:
    out: list[dict] = []
    for batch in reader:
        out.extend(batch.to_pylist())
    return out


def _tables_from_get_objects(rows, prefix: str) -> dict[str, dict]:
    return {
        t["table_name"]: t
        for r in rows
        for s in r.get("catalog_db_schemas") or []
        for t in s.get("db_schema_tables") or []
        if t["table_name"].startswith(prefix)
    }


# --------------------------------------------------------------------------
# README examples
# --------------------------------------------------------------------------


def test_readme_quickstart_step3_connect_and_query(oracle_server):
    """README "Step 3: Connect and query" — the headline example."""
    import adbc_driver_oracle.dbapi as oracle

    with oracle.connect(uri=oracle_server.uri) as conn, conn.cursor() as cur:
        cur.execute("SELECT 42 AS answer, 'hello oracle' AS greeting FROM DUAL")
        table = cur.fetch_arrow_table()
        assert table.num_rows == 1
        assert table.column("ANSWER").to_pylist() == [42]
        assert table.column("GREETING").to_pylist() == ["hello oracle"]


def test_readme_credentials_as_options(oracle_server):
    """README "Prefer to keep credentials out of the URI?" snippet."""
    import adbc_driver_oracle.dbapi as oracle

    with oracle.connect(
        uri=oracle_server.credential_free_uri,
        db_kwargs=oracle_server.db_kwargs,
    ) as conn, conn.cursor() as cur:
        cur.execute("SELECT USER FROM DUAL")
        assert cur.fetchone() == (oracle_server.username.upper(),)


def test_readme_alternative_manager_pattern(oracle_server):
    """README "Alternative: drive adbc_driver_manager directly"."""
    from adbc_driver_manager import dbapi
    import adbc_driver_oracle

    with dbapi.connect(
        driver=adbc_driver_oracle._driver_path(),
        entrypoint="OracleDriverInit",
        db_kwargs={"uri": oracle_server.uri},
    ) as conn, conn.cursor() as cur:
        cur.execute("SELECT 42 AS answer FROM DUAL")
        assert cur.fetch_arrow_table().to_pylist() == [{"ANSWER": 42}]


def test_readme_streaming_large_result_set(oracle_server):
    """README "Streaming large result sets" — fetch_record_batch loop."""
    import adbc_driver_oracle.dbapi as oracle

    with oracle.connect(
        uri=oracle_server.uri,
        db_kwargs={"adbc.oracle.batch_size": "4096"},
    ) as conn, conn.cursor() as cur:
        cur.execute("SELECT LEVEL AS n FROM DUAL CONNECT BY LEVEL <= 50000")
        reader = cur.fetch_record_batch()
        total = 0
        batches = 0
        for batch in reader:
            assert batch.num_rows <= 4096
            total += batch.num_rows
            batches += 1
        assert total == 50_000
        assert batches >= 13


def test_readme_bulk_ingest(oracle_server, table_name):
    """README "Bulk ingest (Arrow → Oracle)" — the autocommit=True snippet,
    verified from a *fresh* connection so a silent rollback would fail."""
    import adbc_driver_oracle.dbapi as oracle

    table = pa.table({"id": [1, 2, 3], "name": ["alice", "bob", "carol"]})
    with oracle.connect(uri=oracle_server.uri, autocommit=True) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.adbc_ingest(table_name=table_name, data=table, mode="create_append")

    try:
        with _connect(oracle_server) as conn, conn.cursor() as cur:
            cur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
            assert cur.fetch_arrow_table().to_pylist()[0]["N"] == 3
    finally:
        with _connect(oracle_server) as conn, conn.cursor() as cur:
            drop_table_quietly(cur, table_name)


def test_readme_bulk_ingest_explicit_commit(oracle_server, table_name):
    """README bulk-ingest "explicit transaction" variant: autocommit-off
    connection + conn.commit() after adbc_ingest. Also pins the documented
    footgun: without the commit the appended rows are rolled back (the
    CREATE TABLE itself survives — Oracle DDL commits implicitly)."""
    import adbc_driver_oracle.dbapi as oracle

    table = pa.table({"id": [1, 2, 3], "name": ["alice", "bob", "carol"]})
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10), name VARCHAR2(50))")
    try:
        # No commit → rows rolled back on close.
        with oracle.connect(uri=oracle_server.uri) as conn, conn.cursor() as cur:
            cur.adbc_ingest(table_name=table_name, data=table, mode="append")
        with _connect(oracle_server) as conn, conn.cursor() as cur:
            cur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
            assert cur.fetch_arrow_table().to_pylist()[0]["N"] == 0

        # With conn.commit() → persists.
        with oracle.connect(uri=oracle_server.uri) as conn, conn.cursor() as cur:
            cur.adbc_ingest(table_name=table_name, data=table, mode="append")
            conn.commit()
        with _connect(oracle_server) as conn, conn.cursor() as cur:
            cur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
            assert cur.fetch_arrow_table().to_pylist()[0]["N"] == 3
    finally:
        with _connect(oracle_server) as conn, conn.cursor() as cur:
            drop_table_quietly(cur, table_name)


def test_readme_transactions(oracle_server, table_name):
    """README "Transactions (autocommit off)" — explicit commit example."""
    import adbc_driver_oracle.dbapi as oracle

    orders = f"{table_name}_ORDERS"
    items = f"{table_name}_ITEMS"
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, orders)
        drop_table_quietly(cur, items)
        cur.execute(f"CREATE TABLE {orders} (id NUMBER(10), status VARCHAR2(20))")
        cur.execute(f"CREATE TABLE {items} (order_id NUMBER(10), name VARCHAR2(20), qty NUMBER(10))")
    try:
        with oracle.connect(uri=oracle_server.uri, autocommit=False) as conn, conn.cursor() as cur:
            cur.execute(f"INSERT INTO {orders} VALUES (1, 'pending')")
            cur.execute(f"INSERT INTO {items} VALUES (1, 'widget', 2)")
            conn.commit()

        with _connect(oracle_server) as conn, conn.cursor() as cur:
            cur.execute(f"SELECT COUNT(*) AS n FROM {orders}")
            assert cur.fetch_arrow_table().to_pylist()[0]["N"] == 1
            cur.execute(f"SELECT COUNT(*) AS n FROM {items}")
            assert cur.fetch_arrow_table().to_pylist()[0]["N"] == 1
    finally:
        with _connect(oracle_server) as conn, conn.cursor() as cur:
            drop_table_quietly(cur, orders)
            drop_table_quietly(cur, items)


def test_readme_parameter_binding(oracle_server, table_name):
    """README "Parameter binding" — qmark placeholders."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (deptno NUMBER(4), ename VARCHAR2(10), sal NUMBER(7,2))")
        try:
            cur.execute(f"INSERT INTO {table_name} VALUES (10, 'KING', 5000)")
            cur.execute(f"INSERT INTO {table_name} VALUES (10, 'CLARK', 1000)")
            cur.execute(f"INSERT INTO {table_name} VALUES (20, 'SMITH', 3000)")
            cur.execute(
                f"SELECT ename, sal FROM {table_name} WHERE deptno = ? AND sal > ? ORDER BY ename",
                (10, 1500),
            )
            rows = cur.fetch_arrow_table().to_pylist()
            assert [r["ENAME"] for r in rows] == ["KING"]
        finally:
            drop_table_quietly(cur, table_name)


# --------------------------------------------------------------------------
# DBAPI basics
# --------------------------------------------------------------------------


def test_connect_and_select(oracle_server):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        cur.execute("SELECT 42 AS answer, 'hello' AS greeting FROM DUAL")
        table = cur.fetch_arrow_table()
        assert table.num_rows == 1
        assert table.column("ANSWER").to_pylist() == [42]
        assert table.column("GREETING").to_pylist() == ["hello"]


def test_fetchone_fetchall(oracle_server):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        cur.execute("SELECT LEVEL AS n FROM DUAL CONNECT BY LEVEL <= 5 ORDER BY 1")
        assert cur.fetchone() == (1,)
        assert cur.fetchall() == [(2,), (3,), (4,), (5,)]


def test_description_after_select_and_ddl(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        cur.execute("SELECT 1 AS a, 'x' AS b FROM DUAL")
        assert [d[0] for d in cur.description] == ["A", "B"]
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10))")
        try:
            assert not cur.description  # no result set: [] (or None)
        finally:
            drop_table_quietly(cur, table_name)


def test_create_insert_select_drop(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10), name VARCHAR2(20))")
        try:
            cur.execute(f"INSERT INTO {table_name} VALUES (1, 'a')")
            cur.execute(f"INSERT INTO {table_name} VALUES (2, 'b')")
            cur.execute(f"INSERT INTO {table_name} VALUES (3, 'c')")
            cur.execute(f"SELECT id, name FROM {table_name} ORDER BY id")
            rows = cur.fetch_arrow_table().to_pylist()
            assert rows == [
                {"ID": 1, "NAME": "a"},
                {"ID": 2, "NAME": "b"},
                {"ID": 3, "NAME": "c"},
            ]
        finally:
            drop_table_quietly(cur, table_name)


def test_update_returns_rowcount(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10), flag NUMBER(1))")
        try:
            for i in range(5):
                cur.execute(f"INSERT INTO {table_name} VALUES ({i}, 0)")
            cur.execute(f"UPDATE {table_name} SET flag = 1 WHERE id >= 2")
            assert cur.rowcount == 3
            cur.execute(f"DELETE FROM {table_name} WHERE flag = 1")
            assert cur.rowcount == 3
        finally:
            drop_table_quietly(cur, table_name)


def test_parameterized_insert_and_select(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(
            f"CREATE TABLE {table_name} (id NUMBER(10), name VARCHAR2(20), amt NUMBER(10,2), d DATE)"
        )
        try:
            cur.execute(
                f"INSERT INTO {table_name} VALUES (?, ?, ?, ?)",
                (7, "seven", decimal.Decimal("12.34"), datetime.datetime(2024, 1, 2, 3, 4, 5)),
            )
            cur.execute(f"SELECT id, name, amt, d FROM {table_name} WHERE id = ?", (7,))
            row = cur.fetch_arrow_table().to_pylist()[0]
            assert row["ID"] == 7
            assert row["NAME"] == "seven"
            assert decimal.Decimal(str(row["AMT"])) == decimal.Decimal("12.34")
            assert row["D"] == datetime.datetime(2024, 1, 2, 3, 4, 5)
        finally:
            drop_table_quietly(cur, table_name)


def test_executemany(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10), name VARCHAR2(20))")
        try:
            cur.executemany(
                f"INSERT INTO {table_name} VALUES (?, ?)",
                [(1, "a"), (2, "b"), (3, "c")],
            )
            cur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
            assert cur.fetchone() == (3,)
        finally:
            drop_table_quietly(cur, table_name)


def test_null_handling(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10), name VARCHAR2(20), d DATE)")
        try:
            cur.execute(f"INSERT INTO {table_name} VALUES (1, NULL, NULL)")
            cur.execute(f"INSERT INTO {table_name} VALUES (NULL, 'x', DATE '2020-01-01')")
            cur.execute(f"SELECT id, name, d FROM {table_name} ORDER BY id NULLS LAST")
            rows = cur.fetch_arrow_table().to_pylist()
            assert rows[0] == {"ID": 1, "NAME": None, "D": None}
            assert rows[1]["ID"] is None and rows[1]["NAME"] == "x"
        finally:
            drop_table_quietly(cur, table_name)


def test_oracle_error_surfaces_ora_code(oracle_server):
    import adbc_driver_oracle.dbapi as oracle

    with _connect(oracle_server) as conn, conn.cursor() as cur:
        with pytest.raises(oracle.Error, match="ORA-00942"):
            cur.execute("SELECT * FROM ADBC_TEST_DOES_NOT_EXIST")
        # Connection must still be usable afterwards.
        cur.execute("SELECT 1 FROM DUAL")
        assert cur.fetchone() == (1,)


def test_unicode_round_trip(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (s NVARCHAR2(50), v VARCHAR2(200))")
        try:
            text = "héllo wörld — 日本語 🦆"
            cur.execute(f"INSERT INTO {table_name} VALUES (?, ?)", (text, text))
            cur.execute(f"SELECT s, v FROM {table_name}")
            row = cur.fetch_arrow_table().to_pylist()[0]
            assert row["S"] == text
            assert row["V"] == text
        finally:
            drop_table_quietly(cur, table_name)


# --------------------------------------------------------------------------
# Types
# --------------------------------------------------------------------------


def test_typed_arrow_columns(oracle_server, table_name):
    """Verify column types map to the Arrow types the README documents."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(
            f"CREATE TABLE {table_name} ("
            "  i NUMBER(10),"
            "  big NUMBER(18),"
            "  dec NUMBER(12,2),"
            "  flt BINARY_FLOAT,"
            "  dbl BINARY_DOUBLE,"
            "  s VARCHAR2(10),"
            "  c CHAR(3),"
            "  r RAW(8),"
            "  d DATE,"
            "  ts TIMESTAMP(6),"
            "  tstz TIMESTAMP(6) WITH TIME ZONE,"
            "  clb CLOB,"
            "  blb BLOB"
            ")"
        )
        try:
            cur.execute(
                f"INSERT INTO {table_name} VALUES ("
                "1, 1234567890123, 12.34, 1.5, 2.5, 'x', 'abc', HEXTORAW('DEADBEEF'),"
                " DATE '2024-01-02', TIMESTAMP '2024-01-02 03:04:05.123456',"
                " TIMESTAMP '2024-01-02 03:04:05.123456 +02:00',"
                " 'clob text', HEXTORAW('00FF'))"
            )
            cur.execute(f"SELECT * FROM {table_name}")
            table = cur.fetch_arrow_table()
            schema = table.schema
            assert schema.field("I").type == pa.int64()
            assert schema.field("BIG").type == pa.int64()
            assert pa.types.is_decimal(schema.field("DEC").type)
            assert schema.field("DEC").type.scale == 2
            assert schema.field("FLT").type == pa.float32()
            assert schema.field("DBL").type == pa.float64()
            assert schema.field("S").type == pa.string()
            assert schema.field("C").type == pa.string()
            assert schema.field("R").type == pa.binary()
            assert pa.types.is_timestamp(schema.field("D").type)
            assert pa.types.is_timestamp(schema.field("TS").type)
            assert pa.types.is_timestamp(schema.field("TSTZ").type)
            assert schema.field("TSTZ").type.tz is not None
            assert schema.field("CLB").type == pa.string()
            assert schema.field("BLB").type == pa.binary()

            row = table.to_pylist()[0]
            assert row["I"] == 1
            assert row["BIG"] == 1234567890123
            assert decimal.Decimal(str(row["DEC"])) == decimal.Decimal("12.34")
            assert row["FLT"] == 1.5
            assert row["DBL"] == 2.5
            assert row["S"] == "x"
            assert row["C"] == "abc"
            assert row["R"] == b"\xde\xad\xbe\xef"
            assert row["D"] == datetime.datetime(2024, 1, 2)
            assert row["TS"] == datetime.datetime(2024, 1, 2, 3, 4, 5, 123456)
            assert row["TSTZ"].astimezone(datetime.timezone.utc) == datetime.datetime(
                2024, 1, 2, 1, 4, 5, 123456, tzinfo=datetime.timezone.utc
            )
            assert row["CLB"] == "clob text"
            assert row["BLB"] == b"\x00\xff"
        finally:
            drop_table_quietly(cur, table_name)


def test_number_edge_cases(oracle_server):
    """Oracle NUMBER's proprietary encoding: negatives, zero, fractions,
    large magnitudes, and many significant digits all decode exactly."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        cur.execute(
            "SELECT CAST(0 AS NUMBER(10)) AS zero,"
            "       CAST(-1 AS NUMBER(10)) AS neg1,"
            "       CAST(-123456789 AS NUMBER(10)) AS negbig,"
            "       CAST(9999999999999999 AS NUMBER(18)) AS big18,"
            "       CAST(-0.01 AS NUMBER(10,2)) AS negfrac,"
            "       CAST(123.456 AS NUMBER(10,3)) AS frac,"
            "       CAST(1E30 AS NUMBER(38,0)) AS huge,"
            "       CAST(0.000001 AS NUMBER(10,6)) AS tiny"
            "  FROM DUAL"
        )
        row = cur.fetch_arrow_table().to_pylist()[0]
        assert row["ZERO"] == 0
        assert row["NEG1"] == -1
        assert row["NEGBIG"] == -123456789
        assert row["BIG18"] == 9999999999999999
        assert decimal.Decimal(str(row["NEGFRAC"])) == decimal.Decimal("-0.01")
        assert decimal.Decimal(str(row["FRAC"])) == decimal.Decimal("123.456")
        assert decimal.Decimal(str(row["HUGE"])) == decimal.Decimal("1E30")
        assert decimal.Decimal(str(row["TINY"])) == decimal.Decimal("0.000001")


def test_unconstrained_number_is_float64(oracle_server):
    """A plain NUMBER expression (no precision) maps to float64 — matches
    the README type table."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        cur.execute("SELECT 1/3 AS third, 2.5 AS lit FROM DUAL")
        table = cur.fetch_arrow_table()
        assert table.schema.field("THIRD").type == pa.float64()
        row = table.to_pylist()[0]
        assert abs(row["THIRD"] - 1 / 3) < 1e-12
        assert row["LIT"] == 2.5


def test_large_strings_and_lobs(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(4), c CLOB, b BLOB)")
        try:
            big = "x" * 100_000
            cur.execute(f"INSERT INTO {table_name} VALUES (?, ?, ?)", (1, big, big.encode()))
            cur.execute(f"SELECT c, b FROM {table_name}")
            row = cur.fetch_arrow_table().to_pylist()[0]
            assert row["C"] == big
            assert row["B"] == big.encode()
        finally:
            drop_table_quietly(cur, table_name)


# --------------------------------------------------------------------------
# Bulk ingest
# --------------------------------------------------------------------------


def test_bulk_ingest_mode_create(oracle_server, table_name):
    """mode='create' (the default) builds the table from the Arrow schema,
    then errors if asked to create the same table again."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        try:
            table = pa.table({"id": pa.array([1, 2], type=pa.int32()), "name": ["a", "b"]})
            cur.adbc_ingest(table_name=table_name, data=table)  # default mode = create
            cur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
            assert cur.fetch_arrow_table().to_pylist()[0]["N"] == 2

            with pytest.raises(Exception):
                cur.adbc_ingest(table_name=table_name, data=table, mode="create")
        finally:
            drop_table_quietly(cur, table_name)


def test_bulk_ingest_mode_append_requires_existing_table(oracle_server, table_name):
    """mode='append' performs no DDL — appending to a missing table errors."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        table = pa.table({"id": pa.array([1], type=pa.int32())})
        with pytest.raises(Exception):
            cur.adbc_ingest(table_name=table_name, data=table, mode="append")

        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10))")
        try:
            cur.adbc_ingest(table_name=table_name, data=table, mode="append")
            cur.adbc_ingest(table_name=table_name, data=table, mode="append")
            cur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
            assert cur.fetch_arrow_table().to_pylist()[0]["N"] == 2
        finally:
            drop_table_quietly(cur, table_name)


def test_bulk_ingest_mode_replace(oracle_server, table_name):
    """mode='replace' drops + recreates the table — old rows and the old
    schema are gone, replaced by the incoming Arrow schema."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        try:
            first = pa.table({"id": pa.array([1, 2, 3], type=pa.int32())})
            cur.adbc_ingest(table_name=table_name, data=first, mode="create")

            second = pa.table({"label": ["x"]})
            cur.adbc_ingest(table_name=table_name, data=second, mode="replace")

            cur.execute(f"SELECT * FROM {table_name}")
            out = cur.fetch_arrow_table()
            assert out.column_names == ["LABEL"]
            assert out.to_pylist() == [{"LABEL": "x"}]
        finally:
            drop_table_quietly(cur, table_name)


def test_bulk_ingest_mode_create_append(oracle_server, table_name):
    """mode='create_append' creates on first call, appends on the next."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        try:
            table = pa.table({"id": pa.array([42], type=pa.int64())})
            cur.adbc_ingest(table_name=table_name, data=table, mode="create_append")
            cur.adbc_ingest(table_name=table_name, data=table, mode="create_append")
            cur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
            assert cur.fetch_arrow_table().to_pylist()[0]["N"] == 2
        finally:
            drop_table_quietly(cur, table_name)


def test_bulk_ingest_record_batch_reader(oracle_server, table_name):
    """adbc_ingest accepts a RecordBatchReader (streamed, multi-batch)."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        try:
            schema = pa.schema([("id", pa.int64()), ("v", pa.float64())])
            batches = [
                pa.record_batch(
                    [pa.array(range(i * 1000, (i + 1) * 1000)), pa.array([0.5] * 1000)],
                    schema=schema,
                )
                for i in range(5)
            ]
            reader = pa.RecordBatchReader.from_batches(schema, batches)
            cur.adbc_ingest(table_name=table_name, data=reader, mode="create")
            cur.execute(f"SELECT COUNT(*) AS n, SUM(id) AS s FROM {table_name}")
            row = cur.fetch_arrow_table().to_pylist()[0]
            assert row["N"] == 5000
            assert int(row["S"]) == sum(range(5000))
        finally:
            drop_table_quietly(cur, table_name)


def test_bulk_ingest_typed_round_trip(oracle_server, table_name):
    """Every Arrow type the DDL generator supports survives create + read."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        try:
            table = pa.table(
                {
                    "i8": pa.array([1, None], type=pa.int8()),
                    "i32": pa.array([-5, 5], type=pa.int32()),
                    "i64": pa.array([2**40, -(2**40)], type=pa.int64()),
                    "f32": pa.array([1.5, None], type=pa.float32()),
                    "f64": pa.array([2.25, -2.25], type=pa.float64()),
                    "b": pa.array([True, False]),
                    "s": pa.array(["hello", None]),
                    "bin": pa.array([b"\x00\x01", b""], type=pa.binary()),
                    "dec": pa.array(
                        [decimal.Decimal("123.45"), decimal.Decimal("-0.01")],
                        type=pa.decimal128(10, 2),
                    ),
                    "d": pa.array([datetime.date(2024, 1, 2), None], type=pa.date32()),
                    "ts": pa.array(
                        [datetime.datetime(2024, 1, 2, 3, 4, 5, 123456), None],
                        type=pa.timestamp("us"),
                    ),
                }
            )
            cur.adbc_ingest(table_name=table_name, data=table, mode="create")
            cur.execute(f"SELECT * FROM {table_name} ORDER BY i32")
            rows = cur.fetch_arrow_table().to_pylist()
            assert len(rows) == 2
            pos, neg = rows  # ORDER BY i32: the i32=-5 row ("pos" values) sorts first
            assert pos["I8"] == 1 and neg["I8"] is None
            assert pos["I64"] == 2**40 and neg["I64"] == -(2**40)
            assert pos["F32"] == 1.5 and neg["F32"] is None
            assert neg["F64"] == -2.25
            assert pos["B"] in (True, 1) and neg["B"] in (False, 0)
            assert pos["S"] == "hello" and neg["S"] is None
            assert pos["BIN"] == b"\x00\x01"
            assert decimal.Decimal(str(pos["DEC"])) == decimal.Decimal("123.45")
            assert pos["TS"] == datetime.datetime(2024, 1, 2, 3, 4, 5, 123456)
            assert neg["TS"] is None
        finally:
            drop_table_quietly(cur, table_name)


def test_bulk_ingest_in_transaction(oracle_server, table_name):
    """Bulk ingest participates in the connection's transaction: with
    autocommit off, ingested rows roll back and only survive on commit."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10))")

    try:
        with _connect(oracle_server, autocommit=False) as conn, conn.cursor() as cur:
            table = pa.table({"id": pa.array([1, 2, 3], type=pa.int32())})

            cur.adbc_ingest(table_name=table_name, data=table, mode="append")
            conn.rollback()
            cur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
            assert cur.fetch_arrow_table().to_pylist()[0]["N"] == 0

            cur.adbc_ingest(table_name=table_name, data=table, mode="append")
            conn.commit()

        with _connect(oracle_server) as conn, conn.cursor() as cur:
            cur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
            assert cur.fetch_arrow_table().to_pylist()[0]["N"] == 3
    finally:
        with _connect(oracle_server) as conn, conn.cursor() as cur:
            drop_table_quietly(cur, table_name)


def test_bulk_ingest_into_other_schema_requires_privilege(oracle_server, table_name):
    """db_schema_name is honoured: targeting a schema we can't write to
    fails with an Oracle error rather than silently landing elsewhere."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        table = pa.table({"id": pa.array([1], type=pa.int32())})
        with pytest.raises(Exception, match="ORA-"):
            cur.adbc_ingest(
                table_name=table_name, data=table, mode="create", db_schema_name="SYS"
            )


# --------------------------------------------------------------------------
# Transactions
# --------------------------------------------------------------------------


def test_transaction_commit_and_rollback(oracle_server, table_name):
    """When autocommit is off, Commit persists writes and Rollback discards them."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10))")

    try:
        with _connect(oracle_server, autocommit=False) as conn, conn.cursor() as cur:
            cur.execute(f"INSERT INTO {table_name} VALUES (1)")
            conn.commit()

            cur.execute(f"INSERT INTO {table_name} VALUES (2)")
            conn.rollback()

            cur.execute(f"INSERT INTO {table_name} VALUES (3)")
            conn.commit()

        with _connect(oracle_server) as conn, conn.cursor() as cur:
            cur.execute(f"SELECT id FROM {table_name} ORDER BY id")
            ids = [r["ID"] for r in cur.fetch_arrow_table().to_pylist()]
            assert ids == [1, 3], f"expected [1, 3] (2 should have rolled back), got {ids}"
    finally:
        with _connect(oracle_server) as conn, conn.cursor() as cur:
            drop_table_quietly(cur, table_name)


def test_uncommitted_rows_invisible_to_other_connection(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10))")
    try:
        with _connect(oracle_server, autocommit=False) as writer, writer.cursor() as wcur:
            wcur.execute(f"INSERT INTO {table_name} VALUES (1)")
            with _connect(oracle_server) as reader, reader.cursor() as rcur:
                rcur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
                assert rcur.fetchone() == (0,)
            writer.commit()
            with _connect(oracle_server) as reader, reader.cursor() as rcur:
                rcur.execute(f"SELECT COUNT(*) AS n FROM {table_name}")
                assert rcur.fetchone() == (1,)
    finally:
        with _connect(oracle_server) as conn, conn.cursor() as cur:
            drop_table_quietly(cur, table_name)


# --------------------------------------------------------------------------
# Metadata
# --------------------------------------------------------------------------


def test_get_info(oracle_server):
    """adbc_get_info() returns a dict mapping info name -> value."""
    with _connect(oracle_server) as conn:
        info = conn.adbc_get_info()
    vendor_name = info.get("vendor_name") or info.get(0)
    driver_name = info.get("driver_name") or info.get(100)
    vendor_version = info.get("vendor_version") or info.get(1)
    assert vendor_name and "Oracle" in str(vendor_name), f"unexpected vendor: {vendor_name!r}"
    assert driver_name and "Oracle" in str(driver_name), f"unexpected driver: {driver_name!r}"
    assert vendor_version, f"vendor_version missing — got {info}"


def test_get_table_types(oracle_server):
    with _connect(oracle_server) as conn:
        types = set(conn.adbc_get_table_types())
    for expected in ("TABLE", "VIEW"):
        assert expected in types, f"expected {expected!r}, got {types}"


def test_get_table_schema(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(
            f"CREATE TABLE {table_name} (i NUMBER(10), v VARCHAR2(30) NOT NULL, d DATE, n NUMBER(8,3))"
        )
        try:
            schema = conn.adbc_get_table_schema(table_name)
            assert [f.name for f in schema] == ["I", "V", "D", "N"]
            assert schema.field("I").type == pa.int64()
            assert schema.field("V").type == pa.string()
            assert not schema.field("V").nullable
            assert schema.field("I").nullable
            assert pa.types.is_timestamp(schema.field("D").type)
            assert pa.types.is_decimal(schema.field("N").type)
            assert schema.field("N").type.precision == 8
            assert schema.field("N").type.scale == 3
        finally:
            drop_table_quietly(cur, table_name)


def test_get_table_schema_with_schema_name(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (i NUMBER(10))")
        try:
            schema = conn.adbc_get_table_schema(
                table_name, db_schema_filter=oracle_server.username.upper()
            )
            assert [f.name for f in schema] == ["I"]
            with pytest.raises(Exception):
                conn.adbc_get_table_schema("ADBC_TEST_NOPE_" + table_name[-6:])
        finally:
            drop_table_quietly(cur, table_name)


def test_get_objects_catalogs_depth(oracle_server):
    with _connect(oracle_server) as conn:
        rows = _rows(conn.adbc_get_objects(depth="catalogs"))
    assert len(rows) >= 1
    for r in rows:
        assert r["catalog_db_schemas"] == []


def test_get_objects_db_schemas_depth_lists_current_user(oracle_server):
    with _connect(oracle_server) as conn:
        rows = _rows(conn.adbc_get_objects(depth="db_schemas"))
    schemas = {
        s["db_schema_name"] for r in rows for s in r.get("catalog_db_schemas") or []
    }
    assert oracle_server.username.upper() in schemas, schemas
    for r in rows:
        for s in r.get("catalog_db_schemas") or []:
            assert s["db_schema_tables"] == []


def test_get_objects_all_depth_lists_tables_and_columns(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10), name VARCHAR2(20))")
        try:
            rows = _rows(conn.adbc_get_objects(depth="all", table_name_filter=table_name))
            by_table = _tables_from_get_objects(rows, table_name)
            assert table_name in by_table, f"{table_name} missing in {list(by_table)!r}"
            ours = by_table[table_name]
            assert ours["table_type"] == "TABLE"
            cols = {c["column_name"]: c for c in ours.get("table_columns") or []}
            assert set(cols) == {"ID", "NAME"}
            assert cols["ID"]["ordinal_position"] == 1
            assert cols["NAME"]["ordinal_position"] == 2
        finally:
            drop_table_quietly(cur, table_name)


def test_get_objects_tables_depth_omits_columns(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10))")
        try:
            rows = _rows(conn.adbc_get_objects(depth="tables", table_name_filter=table_name))
            ours = _tables_from_get_objects(rows, table_name)[table_name]
            assert ours["table_columns"] == []
        finally:
            drop_table_quietly(cur, table_name)


def test_get_objects_column_name_filter(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10), name VARCHAR2(20))")
        try:
            rows = _rows(
                conn.adbc_get_objects(
                    depth="all", table_name_filter=table_name, column_name_filter="NAME"
                )
            )
            ours = _tables_from_get_objects(rows, table_name)[table_name]
            assert [c["column_name"] for c in ours["table_columns"]] == ["NAME"]
        finally:
            drop_table_quietly(cur, table_name)


def test_get_objects_table_type_filter_views(oracle_server, table_name):
    view = f"{table_name}_V"
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER(10))")
        cur.execute(f"CREATE VIEW {view} AS SELECT id FROM {table_name}")
        try:
            rows = _rows(
                conn.adbc_get_objects(
                    depth="tables", table_name_filter=f"{table_name}%", table_types_filter=["VIEW"]
                )
            )
            found = _tables_from_get_objects(rows, table_name)
            assert view in found and found[view]["table_type"] == "VIEW"
            if table_name in found:
                # The Go driver applies the filter (covered by the Go test
                # suite); adbc_driver_manager's Cython layer currently passes
                # NULL for table_types ("TODO: support table_types" in
                # _lib.pyx), so through Python the filter cannot take effect.
                pytest.xfail("adbc_driver_manager does not forward table_types_filter to the driver")
        finally:
            try:
                cur.execute(f"DROP VIEW {view}")
            except Exception:
                pass
            drop_table_quietly(cur, table_name)


def test_get_objects_returns_primary_and_foreign_keys(oracle_server, table_name):
    """table_constraints carries PK + FK info for a parent/child pair."""
    users = f"{table_name}_U"
    orders = f"{table_name}_O"
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, orders)
        drop_table_quietly(cur, users)
        cur.execute(f"CREATE TABLE {users} (id NUMBER(10) PRIMARY KEY, name VARCHAR2(20))")
        cur.execute(
            f"CREATE TABLE {orders} (order_id NUMBER(10) PRIMARY KEY, "
            f"user_id NUMBER(10) REFERENCES {users}(id), amount BINARY_DOUBLE)"
        )
        try:
            rows = _rows(conn.adbc_get_objects(depth="all", table_name_filter=f"{table_name}%"))
            by_table = _tables_from_get_objects(rows, table_name)
            assert users in by_table and orders in by_table, list(by_table)

            users_cs = by_table[users].get("table_constraints") or []
            pks = [c for c in users_cs if c["constraint_type"] == "PRIMARY KEY"]
            assert pks, f"no PK on users: {users_cs!r}"
            assert "ID" in pks[0]["constraint_column_names"]

            orders_cs = by_table[orders].get("table_constraints") or []
            fks = [c for c in orders_cs if c["constraint_type"] == "FOREIGN KEY"]
            assert fks, f"no FK on orders: {orders_cs!r}"
            assert "USER_ID" in fks[0]["constraint_column_names"]
            usages = fks[0].get("constraint_column_usage") or []
            assert any(
                u["fk_table"] == users and u["fk_column_name"] == "ID" for u in usages
            ), f"expected usage referencing {users}.ID, got {usages!r}"
        finally:
            drop_table_quietly(cur, orders)
            drop_table_quietly(cur, users)


# --------------------------------------------------------------------------
# Connection options
# --------------------------------------------------------------------------


def test_application_name_visible_in_session(oracle_server):
    import adbc_driver_oracle.dbapi as oracle

    with oracle.connect(
        uri=oracle_server.uri,
        db_kwargs={"adbc.oracle.application_name": "adbc-it-appname"},
    ) as conn, conn.cursor() as cur:
        cur.execute("SELECT SYS_CONTEXT('USERENV', 'CLIENT_PROGRAM_NAME') AS p FROM DUAL")
        assert cur.fetchone()[0] == "adbc-it-appname"


def test_current_schema_option(oracle_server):
    import adbc_driver_oracle.dbapi as oracle

    with oracle.connect(
        uri=oracle_server.uri,
        db_kwargs={"adbc.oracle.current_schema": "SYS"},
    ) as conn, conn.cursor() as cur:
        cur.execute("SELECT SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA') AS s FROM DUAL")
        assert cur.fetchone() == ("SYS",)


def test_connect_timeout_to_unroutable_host():
    import adbc_driver_oracle.dbapi as oracle

    with pytest.raises(oracle.Error):
        oracle.connect(
            uri="oracle://u:p@10.255.255.1:1521/NOPE",
            db_kwargs={"adbc.oracle.connect_timeout": "1"},
        )


def test_fresh_connections_are_independent(oracle_server):
    with _connect(oracle_server) as a, _connect(oracle_server) as b:
        with a.cursor() as ca, b.cursor() as cb:
            ca.execute("SELECT SYS_CONTEXT('USERENV', 'SID') FROM DUAL")
            cb.execute("SELECT SYS_CONTEXT('USERENV', 'SID') FROM DUAL")
            assert ca.fetchone() != cb.fetchone()


def test_23ai_and_extended_types(oracle_server, table_name):
    """JSON (native OSON, decoded client-side), BOOLEAN, both INTERVALs,
    TIMESTAMP WITH LOCAL TIME ZONE (session TZ is UTC), national character
    types, LONG, FLOAT and ROWID all come back with sensible Arrow types."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        drop_table_quietly(cur, table_name)
        cur.execute(
            f"CREATE TABLE {table_name} ("
            "  j JSON, b BOOLEAN, ids INTERVAL DAY(3) TO SECOND(6), iym INTERVAL YEAR TO MONTH,"
            "  ltz TIMESTAMP(3) WITH LOCAL TIME ZONE, nv NVARCHAR2(20), nc NCHAR(3), ncl NCLOB,"
            "  lng LONG, fl FLOAT, big NUMBER(19)"
            ")"
        )
        try:
            cur.execute(
                f"""INSERT INTO {table_name} VALUES (
                    JSON('{{"a": [1, 2.5, {{"b": "x\\u00e9", "n": null, "t": true}}]}}'), TRUE,
                    INTERVAL '3 04:05:06.789' DAY TO SECOND, INTERVAL '2-3' YEAR TO MONTH,
                    TIMESTAMP '2024-06-01 12:00:00.123 +02:00', N'héllo', N'ab', N'nclob text',
                    'long text', 1.5, 9223372036854775807)"""
            )
            cur.execute(f"SELECT j, b, ids, iym, ltz, nv, nc, ncl, lng, fl, big, ROWID AS rid FROM {table_name}")
            table = cur.fetch_arrow_table()
            schema = table.schema
            assert schema.field("J").type == pa.string()
            assert schema.field("B").type == pa.bool_()
            assert pa.types.is_interval(schema.field("IDS").type)
            assert pa.types.is_interval(schema.field("IYM").type)
            assert schema.field("LTZ").type == pa.timestamp("ms", tz="UTC")
            assert schema.field("NCL").type == pa.string()
            assert schema.field("LNG").type == pa.string()
            assert schema.field("FL").type == pa.float64()
            assert pa.types.is_decimal(schema.field("BIG").type)
            assert schema.field("RID").type == pa.string()
            row = table.to_pylist()[0]
            assert json.loads(row["J"]) == {"a": [1, 2.5, {"b": "xé", "n": None, "t": True}]}
            assert row["B"] is True
            assert row["IDS"].days == 3 and row["IDS"].nanoseconds == (4 * 3600 + 5 * 60 + 6) * 10**9 + 789_000_000
            assert row["IYM"].months == 27
            assert row["LTZ"].replace(tzinfo=None) == datetime.datetime(2024, 6, 1, 10, 0, 0, 123000)
            assert row["NV"] == "héllo" and row["NC"] == "ab " and row["NCL"] == "nclob text"
            assert row["LNG"] == "long text" and row["FL"] == 1.5
            assert row["BIG"] == 9223372036854775807
            assert len(row["RID"]) == 18
        finally:
            drop_table_quietly(cur, table_name)


def test_number_mode_option(oracle_server):
    """adbc.oracle.number_mode switches the NUMBER -> Arrow policy."""
    import adbc_driver_oracle.dbapi as oracle

    sql = "SELECT CAST(7 AS NUMBER(10)) AS i, CAST(12.34 AS NUMBER(10,2)) AS d, 1/3 AS third FROM DUAL"
    expectations = {
        "auto": (pa.int64(), pa.decimal128(10, 2), pa.float64()),
        "decimal": (pa.decimal128(10, 0), pa.decimal128(10, 2), pa.decimal128(38, 10)),
        "double": (pa.float64(), pa.float64(), pa.float64()),
        "string": (pa.string(), pa.string(), pa.string()),
    }
    for mode, types in expectations.items():
        with oracle.connect(
            uri=oracle_server.uri, db_kwargs={"adbc.oracle.number_mode": mode}
        ) as conn, conn.cursor() as cur:
            cur.execute(sql)
            table = cur.fetch_arrow_table()
            assert tuple(table.schema.types) == types, mode
            row = table.to_pylist()[0]
            assert float(row["I"]) == 7 and float(row["D"]) == 12.34


# --------------------------------------------------------------------------
# v0.2: PL/SQL OUT binds, implicit result sets, type modes, cancellation
# --------------------------------------------------------------------------


def test_plsql_out_and_inout_binds(oracle_server):
    """OUT / IN OUT binds are returned as a one-row result set whose fields
    carry ORACLE:parameter_type metadata."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        cur.execute(
            "BEGIN :doubled := :n * 2; :greeting := 'hello ' || :greeting; :when := DATE '2024-01-02'; END;",
            (None, 21, "world", None),
        )
        table = cur.fetch_arrow_table()
        assert table.column_names == ["DOUBLED", "GREETING", "WHEN"]
        assert table.schema.field("GREETING").metadata[b"ORACLE:parameter_type"] == b"IN OUT"
        assert table.schema.field("DOUBLED").metadata[b"ORACLE:parameter_type"] == b"OUT"
        row = table.to_pylist()[0]
        # A bare None placeholder has no Arrow type, so the OUT value comes
        # back as text; bind a typed pyarrow array to get an int64 back.
        assert row["DOUBLED"] == "42"
        assert row["GREETING"] == "hello world"
        assert isinstance(row["WHEN"], str)  # NLS-formatted, e.g. '02-JAN-24' 
        cur.execute(
            "BEGIN :doubled := :n * 2; END;",
            pa.record_batch([pa.array([None], pa.int64()), pa.array([21], pa.int64())], names=["doubled", "n"]),
        )
        assert cur.fetch_arrow_table().to_pylist() == [{"DOUBLED": 42}]


def test_plsql_implicit_result_set(oracle_server):
    """DBMS_SQL.RETURN_RESULT cursors stream back like a query."""
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        cur.execute(
            """DECLARE c SYS_REFCURSOR; BEGIN
                 OPEN c FOR SELECT LEVEL AS n, 'r' || LEVEL AS s FROM DUAL CONNECT BY LEVEL <= 70000;
                 DBMS_SQL.RETURN_RESULT(c); END;"""
        )
        table = cur.fetch_arrow_table()
        assert table.num_rows == 70000
        assert table.column("S")[-1].as_py() == "r70000"


def test_interval_and_date_modes(oracle_server):
    import adbc_driver_oracle.dbapi as oracle

    sql = "SELECT DATE '2024-01-02' AS d, INTERVAL '1 02:03:04.5' DAY TO SECOND AS ds, INTERVAL '2-3' YEAR TO MONTH AS ym FROM DUAL"
    with oracle.connect(
        uri=oracle_server.uri,
        db_kwargs={"adbc.oracle.interval_mode": "duration", "adbc.oracle.date_mode": "date32"},
    ) as conn, conn.cursor() as cur:
        cur.execute(sql)
        table = cur.fetch_arrow_table()
        assert table.schema.field("D").type == pa.date32()
        assert table.schema.field("DS").type == pa.duration("us")  # DAY TO SECOND(6) by default
        row = table.to_pylist()[0]
        assert row["D"] == datetime.date(2024, 1, 2)
        assert row["DS"] == datetime.timedelta(days=1, hours=2, minutes=3, seconds=4.5)
    with oracle.connect(
        uri=oracle_server.uri, db_kwargs={"adbc.oracle.interval_mode": "string"}
    ) as conn, conn.cursor() as cur:
        cur.execute(sql)
        row = cur.fetch_arrow_table().to_pylist()[0]
        assert row["DS"] == "P1DT2H3M4.5S" and row["YM"] == "P2Y3M"


def test_batch_bytes_bounds_batches(oracle_server):
    import adbc_driver_oracle.dbapi as oracle

    with oracle.connect(
        uri=oracle_server.uri, db_kwargs={"adbc.oracle.batch_bytes": "20000"}
    ) as conn, conn.cursor() as cur:
        cur.execute("SELECT RPAD('x', 1000, 'x') AS pad FROM DUAL CONNECT BY LEVEL <= 500")
        batches = list(cur.fetch_record_batch())
        assert sum(b.num_rows for b in batches) == 500
        assert len(batches) > 5


def test_cancel_running_statement(oracle_server):
    """cursor.adbc_cancel() from another thread interrupts a long query."""
    import threading
    import time

    with _connect(oracle_server) as conn, conn.cursor() as cur:
        threading.Timer(0.5, cur.adbc_cancel).start()
        start = time.time()
        with pytest.raises(Exception, match="ORA-01013"):
            cur.execute(
                "SELECT COUNT(*) FROM (SELECT LEVEL FROM DUAL CONNECT BY LEVEL <= 30000) a,"
                " (SELECT LEVEL FROM DUAL CONNECT BY LEVEL <= 30000) b"
            )
            cur.fetchall()
        assert time.time() - start < 10
        cur.execute("SELECT 1 FROM DUAL")
        assert cur.fetchone() == (1,)
