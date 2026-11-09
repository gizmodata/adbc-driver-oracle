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

import secrets

import adbc_drivers_validation.tests.connection as connection_tests
import pytest

from . import oracle


def pytest_generate_tests(metafunc) -> None:
    return connection_tests.generate_tests([oracle.get_quirks()], metafunc)


def _quote(name: str) -> str:
    return '"' + name.replace('"', '""') + '"'


class TestConnection(connection_tests.TestConnection):
    @pytest.fixture(scope="class")
    @classmethod
    def get_objects_table(cls, driver, conn):
        """The upstream fixture creates this table by bulk ingest, with lower-case
        names, and the GetObjects tests then expect those exact names back. This
        driver's ingest stores simple names the way Oracle stores unquoted identifiers
        (upper-cased; README "Bulk ingest"), so create the same table and rows with
        quoted lower-case identifiers in SQL instead. GetObjects is still checked
        against the exact names, filters included."""
        table_name = f"getobjects{secrets.token_hex(8)}"
        table = _quote(table_name)
        with conn.cursor() as cursor:
            cursor.execute(f"DROP TABLE IF EXISTS {table} PURGE")
            cursor.execute(f'CREATE TABLE {table} ("ints" NUMBER(10), "strs" VARCHAR2(4000))')
            for ints, strs in ((1, None), (None, "foo"), (42, "spam")):
                cursor.execute(f"INSERT INTO {table} VALUES (:1, :2)", (ints, strs))
        yield (driver.features.current_catalog, driver.features.current_schema, table_name)
        with conn.cursor() as cursor:
            cursor.execute(f"DROP TABLE IF EXISTS {table} PURGE")

    @pytest.fixture(scope="function")
    @classmethod
    def get_objects_view(cls, driver, conn, get_objects_table):
        """Same as upstream, with the names quoted exactly (see get_objects_table)."""
        catalog_name, db_schema_name, table_name = get_objects_table
        view_name = f"{table_name}view"
        with conn.cursor() as cursor:
            cursor.execute(f"CREATE VIEW {_quote(view_name)} AS SELECT * FROM {_quote(table_name)}")
        yield catalog_name, db_schema_name, view_name
        with conn.cursor() as cursor:
            cursor.execute(f"DROP VIEW {_quote(view_name)}")
