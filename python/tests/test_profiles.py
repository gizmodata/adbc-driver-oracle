"""End-to-end tests for driver-manifest resolution and connection
profiles against a live Oracle Database.

These prove the full chain CI-side: install-manifest output → driver
manager name/scheme/profile resolution → our driver → real server.
"""

from __future__ import annotations

import pytest

pytestmark = pytest.mark.integration


@pytest.fixture()
def manifest_env(tmp_path, monkeypatch):
    """Install the oracle.toml manifest and point the driver manager at it."""
    import adbc_driver_oracle

    drivers = tmp_path / "drivers"
    adbc_driver_oracle.install_manifest(directory=drivers)
    monkeypatch.setenv("ADBC_DRIVER_PATH", str(drivers))
    return tmp_path


def test_connect_by_driver_name(oracle_server, manifest_env):
    """Manifest resolution: driver="oracle", no import-derived path."""
    from adbc_driver_manager import dbapi

    with dbapi.connect(
        driver="oracle",
        db_kwargs={"uri": oracle_server.uri},
    ) as conn, conn.cursor() as cur:
        cur.execute("SELECT 42 AS answer FROM DUAL")
        assert cur.fetchone() == (42,)


def test_connect_by_uri_scheme(oracle_server, manifest_env):
    """Scheme resolution: the oracle:// prefix alone selects the driver."""
    from adbc_driver_manager import dbapi

    with dbapi.connect(uri=oracle_server.uri) as conn, conn.cursor() as cur:
        cur.execute("SELECT 'scheme' AS via FROM DUAL")
        assert cur.fetchone() == ("scheme",)


def test_connect_by_uri_scheme_with_credential_options(oracle_server, manifest_env):
    """Credentials supplied as standard ADBC options, not in the URI."""
    from adbc_driver_manager import dbapi

    with dbapi.connect(
        uri=oracle_server.credential_free_uri, db_kwargs=oracle_server.db_kwargs
    ) as conn, conn.cursor() as cur:
        cur.execute("SELECT USER FROM DUAL")
        assert cur.fetchone() == (oracle_server.username.upper(),)


def test_connect_via_profile(oracle_server, manifest_env, monkeypatch):
    """Profile resolution incl. {{ env_var(...) }} password substitution."""
    from adbc_driver_manager import dbapi

    profiles = manifest_env / "profiles"
    profiles.mkdir()
    (profiles / "oracle_it.toml").write_text(
        f'''\
profile_version = 1
driver = "oracle"

[Options]
uri = "{oracle_server.credential_free_uri}"
username = "{oracle_server.username}"
password = "{{{{ env_var(ORACLE_IT_PASSWORD) }}}}"
''',
        encoding="utf-8",
    )
    monkeypatch.setenv("ADBC_PROFILE_PATH", str(profiles))
    monkeypatch.setenv("ORACLE_IT_PASSWORD", oracle_server.password)

    with dbapi.connect(profile="oracle_it") as conn, conn.cursor() as cur:
        cur.execute("SELECT 'profile' AS via FROM DUAL")
        assert cur.fetchone() == ("profile",)


def test_explicit_options_override_profile(oracle_server, manifest_env, monkeypatch):
    """Options passed in code win over the profile's values."""
    from adbc_driver_manager import dbapi

    profiles = manifest_env / "profiles"
    profiles.mkdir()
    (profiles / "oracle_bad.toml").write_text(
        f'''\
profile_version = 1
driver = "oracle"

[Options]
uri = "{oracle_server.credential_free_uri}"
username = "{oracle_server.username}"
password = "definitely-wrong"
''',
        encoding="utf-8",
    )
    monkeypatch.setenv("ADBC_PROFILE_PATH", str(profiles))

    with dbapi.connect(
        profile="oracle_bad", db_kwargs={"password": oracle_server.password}
    ) as conn, conn.cursor() as cur:
        cur.execute("SELECT 1 FROM DUAL")
        assert cur.fetchone() == (1,)


def test_missing_profile_raises(oracle_server, manifest_env, monkeypatch):
    from adbc_driver_manager import dbapi

    monkeypatch.setenv("ADBC_PROFILE_PATH", str(manifest_env / "nope"))
    with pytest.raises(Exception):
        dbapi.connect(profile="does_not_exist")


def test_bad_password_is_a_clean_error(oracle_server):
    """Auth failure surfaces as a DBAPI error (ORA-01017), not a crash."""
    import adbc_driver_oracle.dbapi as oracle

    with pytest.raises(oracle.Error, match="ORA-01017"):
        oracle.connect(
            uri=oracle_server.credential_free_uri,
            db_kwargs={"username": oracle_server.username, "password": "wrong"},
        )
