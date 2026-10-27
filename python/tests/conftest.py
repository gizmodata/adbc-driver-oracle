"""
pytest fixtures for the adbc-driver-oracle integration tests.

Integration tests run against a live Oracle Database named by
``ADBC_ORACLE_TEST_URI`` (default: the gvenzl/oracle-free container
described in CLAUDE.md, ``oracle://scott:tiger@localhost:1521/FREEPDB1``).
They are auto-skipped when nothing is listening on that host:port, so
``pytest`` still runs cleanly on machines / CI legs without Oracle.

The Oracle -> Hardcharger tests additionally need a Hardcharger server
(https://gizmodata.com/hardcharger): the one named by
``ADBC_ORACLE_TEST_HARDCHARGER_URI`` (CI's container), or else one started from
the ``hardcharger`` PyPI package (``pip install -e ".[test,hardcharger]"``).
A server named by that variable must answer, or the tests fail: they never
skip because a configured server is down.
"""

from __future__ import annotations

import os
import socket
import time
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


HARDCHARGER_MIN_VERSION = (2, 1)


@dataclass
class HardchargerServer:
    """Connection details for a Hardcharger server (test-only dependency)."""

    uri: str
    username: str
    password: str
    # The Oracle driver library as the server's own file system sees it (a
    # bind mount in CI's container); None: the local build, for a local server.
    driver_path: str | None = None

    def connect(self, autocommit: bool = False):
        from adbc_driver_hardcharger import dbapi as hardcharger

        return hardcharger.connect(
            uri=self.uri, username=self.username, password=self.password, autocommit=autocommit
        )


def _wait_for_hardcharger(server: HardchargerServer, timeout: float = 120.0) -> None:
    deadline = time.monotonic() + timeout
    last: Exception | None = None
    while time.monotonic() < deadline:
        try:
            with server.connect() as conn, conn.cursor() as cur:
                cur.execute(operation="SELECT HARDCHARGER_VERSION()")
                (version,) = cur.fetchone()
        except Exception as exc:  # not up yet
            last = exc
            time.sleep(2)
            continue
        major_minor = tuple(int(part) for part in version.split(".")[:2])
        if major_minor < HARDCHARGER_MIN_VERSION:
            pytest.fail(reason=f"Hardcharger at {server.uri} is {version}; these tests need 2.1 or later")
        return
    pytest.fail(reason=f"no Hardcharger answering at {server.uri} after {timeout:.0f}s: {last}")


@pytest.fixture(scope="session")
def hardcharger_server():
    uri = os.environ.get("ADBC_ORACLE_TEST_HARDCHARGER_URI")
    if uri:
        server = HardchargerServer(
            uri=uri,
            username=os.environ.get("ADBC_ORACLE_TEST_HARDCHARGER_USERNAME", "hardcharger"),
            password=os.environ.get("ADBC_ORACLE_TEST_HARDCHARGER_PASSWORD", "hardcharger"),
            driver_path=os.environ.get("ADBC_ORACLE_TEST_HARDCHARGER_DRIVER_PATH"),
        )
        _wait_for_hardcharger(server=server)
        yield server
        return
    hardcharger = pytest.importorskip(
        "hardcharger", reason="no ADBC_ORACLE_TEST_HARDCHARGER_URI and the hardcharger package isn't installed"
    )
    pytest.importorskip("adbc_driver_hardcharger")
    with hardcharger.Server() as local:
        server = HardchargerServer(uri=local.uri, username=local.username, password=local.password)
        _wait_for_hardcharger(server=server)
        yield server
