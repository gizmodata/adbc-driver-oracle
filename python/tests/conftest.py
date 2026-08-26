"""
pytest fixtures for the adbc-driver-oracle integration tests.

Integration tests run against a live Oracle Database named by
``ADBC_ORACLE_TEST_URI`` (default: the gvenzl/oracle-free container
described in CLAUDE.md, ``oracle://scott:tiger@localhost:1521/FREEPDB1``).
They are auto-skipped when nothing is listening on that host:port, so
``pytest`` still runs cleanly on machines / CI legs without Oracle.

The Oracle -> GizmoSQL test additionally needs ``GIZMOSQL_URI`` (and
``GIZMOSQL_TOKEN`` or ``GIZMOSQL_USERNAME`` / ``GIZMOSQL_PASSWORD``).
"""

from __future__ import annotations

import os
import socket
import uuid
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlparse

import pytest

DEFAULT_URI = "oracle://scott:tiger@localhost:1521/FREEPDB1"


@dataclass
class OracleServer:
    """Connection details for the Oracle test database."""

    uri: str
    host: str
    port: int
    username: str
    password: str

    @property
    def credential_free_uri(self) -> str:
        """The URI without ``user:password@`` (credentials go via options)."""
        u = urlparse(self.uri)
        netloc = f"{self.host}:{self.port}"
        return u._replace(netloc=netloc).geturl()

    @property
    def db_kwargs(self) -> dict[str, str]:
        return {"username": self.username, "password": self.password}


def _reachable(host: str, port: int, timeout: float = 2.0) -> bool:
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return True
    except OSError:
        return False


@pytest.fixture(scope="session")
def oracle_server() -> OracleServer:
    uri = os.environ.get("ADBC_ORACLE_TEST_URI", DEFAULT_URI)
    parsed = urlparse(uri)
    host = parsed.hostname or "localhost"
    port = parsed.port or 1521
    if not _reachable(host, port):
        pytest.skip(reason=f"no Oracle Database listening at {host}:{port}")
    return OracleServer(
        uri=uri,
        host=host,
        port=port,
        username=parsed.username or "scott",
        password=parsed.password or "tiger",
    )


@pytest.fixture(scope="session")
def driver_path() -> str:
    """Resolve the bundled Oracle ADBC driver library path."""
    candidate = os.environ.get("ADBC_ORACLE_LIBRARY")
    if candidate and Path(candidate).is_file():
        return candidate
    from adbc_driver_oracle import _driver_path

    return _driver_path()


@pytest.fixture()
def table_name() -> str:
    """A unique, upper-case, Oracle-safe test table name."""
    return f"ADBC_TEST_{uuid.uuid4().hex[:12].upper()}"


def drop_table_quietly(cur, name: str) -> None:
    """``DROP TABLE ... PURGE`` ignoring ORA-00942 (table does not exist)."""
    try:
        cur.execute(f"DROP TABLE {name} PURGE")
    except Exception:
        pass


@dataclass
class GizmoSQLServer:
    """Connection details for a GizmoSQL server (test-only dependency)."""

    uri: str
    username: str
    password: str


@pytest.fixture(scope="session")
def gizmosql_server() -> GizmoSQLServer:
    uri = os.environ.get("GIZMOSQL_URI")
    if not uri:
        pytest.skip(reason="GIZMOSQL_URI not set")
    token = os.environ.get("GIZMOSQL_TOKEN")
    if token:
        return GizmoSQLServer(uri=uri, username="token", password=token)
    return GizmoSQLServer(
        uri=uri,
        username=os.environ.get("GIZMOSQL_USERNAME", "gizmosql_user"),
        password=os.environ.get("GIZMOSQL_PASSWORD", "gizmosql_password"),
    )
