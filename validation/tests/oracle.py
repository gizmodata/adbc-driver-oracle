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

import contextlib
import functools
import os
import re
import urllib.parse
from pathlib import Path

from adbc_drivers_validation import model

# The suite connects with ADBC_ORACLE_TEST_URI (oracle://user:password@host:port/service).
# Oracle's schema for a session is its user, stored upper-cased.
URI_ENV = "ADBC_ORACLE_TEST_URI"
# The second schema the schema-qualified tests write to; conftest.py creates it with
# ADBC_ORACLE_TEST_ADMIN_URI.
SECONDARY_SCHEMA = "ADBC_VALIDATION_SECONDARY"


def current_user() -> str:
    uri = os.environ.get(URI_ENV, "")
    return (urllib.parse.urlsplit(uri).username or "").upper()


# Oracle's reserved words (V$RESERVED_WORDS where RESERVED = 'Y'); the driver's
# identFor (driver/oracle/arrow_conv.go) quotes these. Keep the two lists in sync.
RESERVED_WORDS = frozenset(
    """
    ACCESS ADD ALL ALTER AND ANY AS ASC AUDIT BETWEEN BY CHAR CHECK CLUSTER COLUMN COMMENT COMPRESS
    CONNECT CREATE CURRENT DATE DECIMAL DEFAULT DELETE DESC DISTINCT DROP ELSE EXCLUSIVE EXISTS FILE
    FLOAT FOR FROM GRANT GROUP HAVING IDENTIFIED IMMEDIATE IN INCREMENT INDEX INITIAL INSERT INTEGER
    INTERSECT INTO IS LEVEL LIKE LOCK LONG MAXEXTENTS MINUS MLSLABEL MODE MODIFY NOAUDIT NOCOMPRESS
    NOT NOWAIT NULL NUMBER OF OFFLINE ON ONLINE OPTION OR ORDER PCTFREE PRIOR PUBLIC RAW RENAME
    RESOURCE REVOKE ROW ROWID ROWNUM ROWS SELECT SESSION SET SHARE SIZE SMALLINT START SUCCESSFUL
    SYNONYM SYSDATE TABLE THEN TO TRIGGER UID UNION UNIQUE UPDATE USER VALIDATE VALUES VARCHAR
    VARCHAR2 VIEW WHENEVER WHERE WITH
    """.split()
)
_SIMPLE_IDENTIFIER = re.compile(r"[A-Za-z][A-Za-z0-9_$#]{0,127}")


def folds_to_upper(identifier: str) -> bool:
    """True when Oracle (and this driver's ingest DDL) treats the name as an unquoted
    identifier, which the database stores upper-cased."""
    return bool(_SIMPLE_IDENTIFIER.fullmatch(identifier)) and identifier.upper() not in RESERVED_WORDS


class OracleQuirks(model.DriverQuirks):
    name = "oracle"
    driver = "adbc_driver_oracle"
    driver_name = "ADBC Oracle Driver - Go"
    vendor_name = "Oracle Database"
    vendor_version = re.compile(r"23\.[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+")
    short_version = "23"
    # Every unset feature is one the driver does not implement (catalogs: an Oracle
    # session is attached to one database, so there is no second catalog to switch to;
    # GetStatistics: not implemented).
    features = model.DriverFeatures(
        connection_get_table_schema=True,
        connection_set_current_schema=True,
        connection_transactions=True,
        get_objects=True,
        get_objects_constraints_check=True,
        get_objects_constraints_foreign=True,
        get_objects_constraints_primary=True,
        get_objects_constraints_unique=True,
        get_objects_tabletypes_filter=True,
        statement_bind=True,
        statement_bulk_ingest=True,
        statement_bulk_ingest_schema=True,
        statement_bulk_ingest_temporary=True,
        statement_execute_schema=True,
        statement_get_parameter_schema=True,
        statement_prepare=True,
        statement_rows_affected=True,
        # Oracle reports 0 rows for DDL; the driver passes that through.
        statement_rows_affected_ddl=True,
        # The driver sets ORACLE:type on every result field.
        metadata_type_name=True,
        # Temporary ingest creates a GLOBAL TEMPORARY table, which lives in the same
        # namespace as ordinary tables.
        quirk_bulk_ingest_temporary_shares_namespace=True,
        # The database name of the gvenzl/oracle-free image (the PDB FREEPDB1 is in it).
        current_catalog="FREE",
        current_schema=current_user(),
        secondary_schema=SECONDARY_SCHEMA,
        # GetObjects fills every xdbc_* column field; the suite checks these two.
        supported_xdbc_fields=["xdbc_nullable", "xdbc_is_nullable"],
    )
    setup = model.DriverSetup(
        database={
            "uri": model.FromEnv("ADBC_ORACLE_TEST_URI"),
        },
        connection={},
        statement={},
    )

    @property
    def queries_paths(self) -> tuple[Path]:
        return (Path(__file__).parent.parent / "queries" / "oracle",)

    def bind_parameter(self, index: int) -> str:
        return f":{index}"

    def drop_table(
        self,
        *,
        table_name: str,
        schema_name: str | None = None,
        catalog_name: str | None = None,
        if_exists: bool = True,
        temporary: bool = False,
    ) -> str:
        name = self.quote_identifier(*[part for part in (schema_name, table_name) if part])
        if if_exists:
            return f"DROP TABLE IF EXISTS {name} PURGE"
        return f"DROP TABLE {name} PURGE"

    def is_table_not_found(self, table_name: str | None, error: Exception) -> bool:
        # ORA-00942: table or view does not exist (23ai: "... <name> does not exist")
        message = str(error)
        if "ORA-00942" not in message:
            return False
        return table_name is None or table_name.upper() in message.upper()

    @property
    def sample_ddl_constraints(self) -> list[str]:
        # Quoted so the names stay lower-case, as the suite expects them back
        # (Oracle upper-cases unquoted names). Dropped first, children before parents.
        tables = (
            "constraint_foreign_multi",
            "constraint_foreign",
            "constraint_check",
            "constraint_unique",
            "constraint_primary_multi2",
            "constraint_primary_multi",
            "constraint_primary",
        )
        return [f'DROP TABLE IF EXISTS "{t}" PURGE' for t in tables] + [
            'CREATE TABLE "constraint_check" ("a" NUMBER(10), "b" NUMBER(10), '
            'CHECK ("a" > 0), CHECK ("a" + "b" > 0))',
            'CREATE TABLE "constraint_primary" ("a" NUMBER(10), "b" VARCHAR2(100), PRIMARY KEY ("a"))',
            'CREATE TABLE "constraint_primary_multi" ("a" NUMBER(10), "b" VARCHAR2(100), PRIMARY KEY ("b", "a"))',
            'CREATE TABLE "constraint_primary_multi2" ("a" VARCHAR2(100), "b" NUMBER(10), PRIMARY KEY ("a", "b"))',
            'CREATE TABLE "constraint_unique" ("a" NUMBER(10), "b" NUMBER(10), "c" NUMBER(10), '
            'UNIQUE ("a"), UNIQUE ("c", "b"))',
            'CREATE TABLE "constraint_foreign" ("a" NUMBER(10), "b" NUMBER(10), '
            'FOREIGN KEY ("b") REFERENCES "constraint_primary" ("a"))',
            'CREATE TABLE "constraint_foreign_multi" ("a" NUMBER(10), "b" NUMBER(10), "c" VARCHAR2(100), '
            'FOREIGN KEY ("c", "b") REFERENCES "constraint_primary_multi2" ("a", "b"))',
        ]

    def query_override(self, context: str, default: str) -> str:
        # Oracle has no length-less VARCHAR: give it VARCHAR2's maximum.
        if context in {
            "TestStatement.sample_table",
            "TestConnection.test_get_table_schema_schema",
        }:
            return default.replace("VARCHAR)", "VARCHAR2(4000))")
        if context == "TestIngest.test_schema":
            # The case's int64 `idx` is stored as NUMBER(19), which reads back as
            # decimal128(19, 0); read it as NUMBER(18) to compare with the int64 input.
            return default.replace("SELECT IDX,", "SELECT CAST(IDX AS NUMBER(18)) AS IDX,")
        return super().query_override(context, default)

    @contextlib.contextmanager
    def setup_statement(self, query, cursor):
        if query == "TestIngest.test_create_long_values.ingest":
            # Ingest DDL defaults to VARCHAR2(4000) / RAW(2000), Oracle's in-row
            # limits, and the case writes values up to 128 KiB. The driver's documented
            # options choose LOB columns instead (README "Bulk ingest").
            cursor.adbc_statement.set_options(
                **{
                    "adbc.oracle.ingest.string_type": "CLOB",
                    "adbc.oracle.ingest.binary_type": "BLOB",
                }
            )
            yield
            return
        with super().setup_statement(query, cursor):
            yield

    def qualify_temp_table(self, cursor, name: str) -> list[str]:
        # A GLOBAL TEMPORARY table is an ordinary schema object (its rows are
        # session-private); ingest stores the name the way Oracle stores it.
        return [name.upper() if folds_to_upper(name) else name]

    def quote_one_identifier(self, identifier: str) -> str:
        # The suite writes names unquoted in its setup SQL (so Oracle stores them
        # upper-cased) and the driver's ingest DDL folds simple names the same way
        # (README "Bulk ingest"). Refer to such names the way Oracle stored them.
        if folds_to_upper(identifier):
            return identifier.upper()
        return super().quote_one_identifier(identifier)

    def split_statement(self, statement: str) -> list[str]:
        # Oracle takes one statement per call, without the trailing ";". Split on
        # ";" outside string literals and drop "--" comments (also outside literals).
        statements, current, in_string, i = [], [], False, 0
        while i < len(statement):
            c = statement[i]
            if in_string:
                current.append(c)
                if c == "'":
                    in_string = False
            elif c == "'":
                in_string = True
                current.append(c)
            elif statement.startswith("--", i):
                while i < len(statement) and statement[i] != "\n":
                    i += 1
                continue
            elif c == ";":
                statements.append("".join(current))
                current = []
            else:
                current.append(c)
            i += 1
        statements.append("".join(current))
        return [s.strip() for s in statements if s.strip()]


@functools.cache
def get_quirks(version: str = OracleQuirks.short_version) -> OracleQuirks:
    quirks_ = OracleQuirks()
    if version != quirks_.short_version:
        raise ValueError(f"unsupported Oracle version: {version}")
    return quirks_
