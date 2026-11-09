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

import os
import sys
from pathlib import Path

import adbc_driver_manager.dbapi
import adbc_drivers_validation.model
import adbc_drivers_validation.tests.conftest
import pyarrow
import pytest
from adbc_drivers_validation import compare
from adbc_drivers_validation.tests.conftest import (  # noqa: F401
    conn,
    conn_factory,
    manual_test,
    noci,
    pytest_collection_modifyitems,
)

from . import oracle

REPO_ROOT = Path(__file__).resolve().parent.parent.parent


# Oracle identifier case. The suite's fixtures name tables and columns with unquoted
# lower-case identifiers and expect them back verbatim; Oracle stores an unquoted
# identifier upper-cased, so `CREATE TABLE t (idx INTEGER)` yields a column IDX. Before
# the suite compares an actual schema with an expected one, rename an actual top-level
# field to the expected name ONLY when it is exactly the upper-case form of an expected
# name Oracle folds (oracle.folds_to_upper). Any other name difference still fails.
def _fold_names(expected: pyarrow.Schema, actual):
    if isinstance(actual, pyarrow.Table):
        schema = _fold_names(expected, actual.schema)
        return actual if schema is actual.schema else actual.rename_columns(schema.names)
    if len(expected) != len(actual):
        return actual
    names = []
    changed = False
    for want, got in zip(expected.names, actual.names):
        if got != want and oracle.folds_to_upper(want) and got == want.upper():
            names.append(want)
            changed = True
        else:
            names.append(got)
    if not changed:
        return actual
    return pyarrow.schema([field.with_name(name) for field, name in zip(actual, names)], metadata=actual.metadata)


_compare_tables = compare.compare_tables
_compare_schemas = compare.compare_schemas


def _compare_tables_folded(expected, actual, meta=None):
    return _compare_tables(expected, _fold_names(expected.schema, actual), meta)


def _compare_schemas_folded(expected, actual):
    return _compare_schemas(expected, _fold_names(expected, actual))


compare.compare_tables = _compare_tables_folded
compare.compare_schemas = _compare_schemas_folded


def pytest_addoption(parser):
    adbc_drivers_validation.tests.conftest.pytest_addoption(parser)
    parser.addoption("--vendor-version", action="store", default=oracle.OracleQuirks.short_version)


@pytest.fixture(scope="session")
def driver(request, pytestconfig) -> adbc_drivers_validation.model.DriverQuirks:
    assert request.param.startswith("oracle")
    return oracle.get_quirks(pytestconfig.getoption("vendor_version"))


@pytest.fixture(scope="session")
def driver_path(driver: adbc_drivers_validation.model.DriverQuirks) -> str:
    # ADBC_ORACLE_LIBRARY is the same variable the Python package's build uses.
    if path := os.environ.get("ADBC_ORACLE_LIBRARY"):
        library = Path(path)
    else:
        ext = {"win32": "dll", "darwin": "dylib"}.get(sys.platform, "so")
        library = REPO_ROOT / "pkg" / "oracle" / f"libadbc_driver_oracle.{ext}"
    if not library.is_file():
        pytest.fail(f"driver library not found: {library} (build it with `make -C pkg/oracle`)")
    return str(library)


@pytest.fixture(scope="session")
def db_kwargs(driver: adbc_drivers_validation.model.DriverQuirks) -> dict[str, str]:
    # The upstream fixture skips every test when a variable is unset. This suite gates
    # releases, so a missing server setting must fail the run instead.
    kwargs = {}
    for key, value in driver.setup.database.items():
        if isinstance(value, adbc_drivers_validation.model.FromEnv):
            resolved = value.get()
            if resolved is None:
                if value.optional:
                    continue
                pytest.fail(f"{value.env} must be set (see validation/README.md)")
            kwargs[key] = resolved
        else:
            kwargs[key] = value
    return kwargs


@pytest.fixture(scope="session", autouse=True)
def secondary_schema(driver_path: str) -> str:
    """Create the second schema the schema-qualified cases use, and let the test user
    write to it. Needs a DBA login (ADBC_ORACLE_TEST_ADMIN_URI, e.g. SYSTEM on the
    same PDB); idempotent."""
    admin_uri = os.environ.get("ADBC_ORACLE_TEST_ADMIN_URI")
    if not admin_uri:
        pytest.fail("ADBC_ORACLE_TEST_ADMIN_URI must be set (see validation/README.md)")
    user = oracle.current_user()
    if not user:
        pytest.fail(f"{oracle.URI_ENV} must be set and name a user (see validation/README.md)")
    statements = [
        f"CREATE USER IF NOT EXISTS {oracle.SECONDARY_SCHEMA} NO AUTHENTICATION QUOTA UNLIMITED ON USERS",
        f"GRANT CREATE ANY TABLE, DROP ANY TABLE, ALTER ANY TABLE, SELECT ANY TABLE, "
        f"INSERT ANY TABLE, UPDATE ANY TABLE, DELETE ANY TABLE TO {user}",
    ]
    with adbc_driver_manager.dbapi.connect(
        driver=driver_path, db_kwargs={"uri": admin_uri}, autocommit=True
    ) as conn, conn.cursor() as cursor:
        for statement in statements:
            cursor.execute(statement)
    return oracle.SECONDARY_SCHEMA


def pytest_sessionfinish(session: pytest.Session, exitstatus: int) -> None:
    # A gating suite that ran nothing must not pass.
    reporter = session.config.pluginmanager.get_plugin("terminalreporter")
    passed = len(reporter.stats.get("passed", [])) if reporter else 0
    if exitstatus == 0 and passed == 0:
        session.exitstatus = pytest.ExitCode.NO_TESTS_COLLECTED
