# Copyright (c) 2025-2026 ADBC Drivers Contributors
# Modifications Copyright (c) 2026 GizmoData LLC (adapted for adbc-driver-oracle)
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#         http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import adbc_drivers_validation.tests.ingest as ingest_tests
import pyarrow
from adbc_drivers_validation import compare
from adbc_drivers_validation.utils import execute_query_without_prepare

from . import oracle


def pytest_generate_tests(metafunc) -> None:
    return ingest_tests.generate_tests([oracle.get_quirks()], metafunc)


class TestIngest(ingest_tests.TestIngest):
    def test_temporary(self, driver, conn_factory, query) -> None:
        """Oracle's temporary tables are GLOBAL TEMPORARY tables: the definition is
        an ordinary schema object that outlives the session and only the rows are
        private to it. So instead of upstream's "the table is gone in a new session",
        check what Oracle guarantees: the creating session reads its rows back, and a
        new session sees the table empty."""
        subquery = query.query
        data = subquery.input()
        expected = subquery.expected()
        table_name = ingest_tests.make_table_name("test_ingest_temporary", query)
        idx = driver.quote_identifier("idx")
        value = driver.quote_identifier("value")
        temp_table = None

        with conn_factory() as conn, conn.cursor() as cursor:
            driver.try_drop_table(cursor, table_name=table_name)
            cursor.adbc_ingest(table_name, data, temporary=True)
            temp_table = driver.quote_identifier(*driver.qualify_temp_table(cursor, table_name))
            select = f"SELECT {idx}, {value} FROM {temp_table} ORDER BY {idx} ASC"
            compare.compare_tables(expected, execute_query_without_prepare(cursor, select))

        with conn_factory() as conn, conn.cursor() as cursor:
            other_session = execute_query_without_prepare(cursor, select)
            assert other_session.num_rows == 0
            driver.try_drop_table(cursor, table_name=table_name)

    def test_not_null(self, driver, conn) -> None:
        """Upstream's check with the names as this driver's ingest stores them: simple
        names upper-cased, like unquoted Oracle identifiers (README "Bulk ingest").
        A non-nullable Arrow field must become a NOT NULL column."""
        table_name = "test_ingest_not_null"
        data = pyarrow.Table.from_pydict(
            {"idx": [1, 2, 3], "value": ["foo", "bar", "baz"]},
            schema=pyarrow.schema(
                [pyarrow.field("idx", pyarrow.int64()), pyarrow.field("value", pyarrow.string(), nullable=False)]
            ),
        )
        with conn.cursor() as cursor:
            driver.try_drop_table(cursor, table_name=table_name)
            cursor.adbc_ingest(table_name, data, mode="create")

        stored = table_name.upper()
        objects = (
            conn.adbc_get_objects(
                depth="columns",
                catalog_filter=driver.features.current_catalog,
                db_schema_filter=driver.features.current_schema,
                table_name_filter=stored,
            )
            .read_all()
            .to_pylist()
        )
        columns = [
            {"table": table["table_name"], **column}
            for obj in objects
            for schema in obj["catalog_db_schemas"]
            for table in schema["db_schema_tables"]
            for column in table["table_columns"]
        ]
        assert len(columns) == 2
        compare.match_fields(
            columns[0],
            {"table": stored, "column_name": "IDX", "ordinal_position": 1, "xdbc_nullable": 1, "xdbc_is_nullable": "YES"},
        )
        compare.match_fields(
            columns[1],
            {"table": stored, "column_name": "VALUE", "ordinal_position": 2, "xdbc_nullable": 0, "xdbc_is_nullable": "NO"},
        )
