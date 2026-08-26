"""Oracle -> GizmoSQL streaming ingest: the customer-notebook flow, ADBC to
ADBC.

Pulls a result set out of Oracle as an Arrow ``RecordBatchReader`` (one
batch of rows at a time, so client memory stays bounded) and hands the
reader straight to GizmoSQL's ``adbc_ingest`` — no pandas, no ODBC, no
Oracle Client libraries.

Needs both an Oracle (``ADBC_ORACLE_TEST_URI``) and a GizmoSQL
(``GIZMOSQL_URI`` plus ``GIZMOSQL_TOKEN`` or ``GIZMOSQL_USERNAME`` /
``GIZMOSQL_PASSWORD``). GizmoSQL is a *test-only* dependency: the driver
itself has no GizmoSQL code.
"""

from __future__ import annotations

import pytest

from .conftest import drop_table_quietly

pytestmark = [pytest.mark.integration, pytest.mark.gizmosql]


def test_stream_oracle_table_into_gizmosql(oracle_server, gizmosql_server, table_name):
    import adbc_driver_oracle.dbapi as oracle

    gizmosql = pytest.importorskip("adbc_driver_gizmosql.dbapi")

    rows = 250_000
    with oracle.connect(
        uri=oracle_server.uri, db_kwargs={"adbc.oracle.batch_size": "65536"}
    ) as src, src.cursor() as src_cur:
        drop_table_quietly(src_cur, table_name)
        src_cur.execute(
            f"CREATE TABLE {table_name} (id NUMBER(10) NOT NULL, name VARCHAR2(40), amt NUMBER(12,2), ts TIMESTAMP(6))"
        )
        src_cur.execute(
            f"""INSERT INTO {table_name} (id, name, amt, ts)
                SELECT LEVEL, 'name-' || LEVEL, LEVEL * 0.25, SYSTIMESTAMP
                FROM DUAL CONNECT BY LEVEL <= {rows}"""
        )
        try:
            # Exactly the notebook's shape: SELECT from Oracle, ingest into GizmoSQL.
            src_cur.execute(f"SELECT id, name, amt, ts FROM {table_name}")
            reader = src_cur.fetch_record_batch()  # streams; 65k rows per batch

            dest = table_name.lower()
            with gizmosql.connect(
                gizmosql_server.uri,
                username=gizmosql_server.username,
                password=gizmosql_server.password,
            ) as dst, dst.cursor() as dst_cur:
                loaded = dst_cur.adbc_ingest(table_name=dest, data=reader, mode="replace")
                dst.commit()
                assert loaded == rows
                dst_cur.execute(f"SELECT COUNT(*), SUM(id), MAX(name), SUM(amt) FROM {dest}")
                count, total, max_name, amt_sum = dst_cur.fetchone()
                assert count == rows
                assert total == rows * (rows + 1) // 2
                assert max_name == "name-99999"
                assert float(amt_sum) == rows * (rows + 1) // 2 * 0.25
                dst_cur.execute(f"DROP TABLE {dest}")
        finally:
            drop_table_quietly(src_cur, table_name)
