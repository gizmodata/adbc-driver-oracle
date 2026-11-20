"""Option names per the ADBC driver style guide, and the pre-1.1.0 names.

The driver's own options are ``oracle.*`` since 1.1.0. The old ``adbc.oracle.*``
names still work through the C library, but the first use of each one in a
process prints ``[oracle] option "<old>" is deprecated; use "<new>"`` to stderr,
naming the keys only, never the value. The warning is once per process, so each
check runs in a child process with its own copy of the library.
"""

from __future__ import annotations

import os
import subprocess
import sys
import textwrap

import pytest

SECRET = "pa55-W0rd-that-must-not-leak"


def _run(code: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        args=[sys.executable, "-c", textwrap.dedent(code)],
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )


def test_old_names_warn_once_and_never_show_values():
    # No database is needed: options are checked and stored when the database is
    # created; nothing connects.
    proc = _run(
        f"""
        import adbc_driver_manager
        import adbc_driver_oracle

        db = adbc_driver_manager.AdbcDatabase(
            driver=adbc_driver_oracle._driver_path(),
            entrypoint="AdbcDriverOracleInit",
            uri="oracle://u@127.0.0.1:1/none",
            **{{"adbc.oracle.batch_size": "123", "adbc.oracle.token": {SECRET!r}}},
        )
        # Read back under the old and the new name; the old name warns only once.
        assert db.get_option("adbc.oracle.batch_size") == "123"
        assert db.get_option("oracle.query.batch_size") == "123"
        db.set_options(**{{"adbc.oracle.batch_size": "456"}})
        assert db.get_option("oracle.query.batch_size") == "456"
        # Separate calls: the last write wins under either name (a later old-name
        # call is not ignored).
        db.set_options(**{{"oracle.query.number_type": "decimal"}})
        db.set_options(**{{"adbc.oracle.number_mode": "string"}})
        assert db.get_option("oracle.query.number_type") == "string"
        db.set_options(**{{"oracle.query.number_type": "decimal"}})
        assert db.get_option("adbc.oracle.number_mode") == "decimal"
        db.close()
        print("ok")
        """
    )
    assert proc.returncode == 0, proc.stderr
    assert proc.stdout.strip() == "ok"
    lines = [line for line in proc.stderr.splitlines() if line.strip()]
    assert sorted(lines) == sorted(
        [
            '[oracle] option "adbc.oracle.batch_size" is deprecated; use "oracle.query.batch_size"',
            '[oracle] option "adbc.oracle.token" is deprecated; use "oracle.auth.token"',
            '[oracle] option "adbc.oracle.number_mode" is deprecated; use "oracle.query.number_type"',
        ]
    ), proc.stderr
    assert SECRET not in proc.stderr
    assert "pa55" not in proc.stderr


def test_new_names_do_not_warn():
    proc = _run(
        """
        import adbc_driver_manager
        import adbc_driver_oracle

        db = adbc_driver_manager.AdbcDatabase(
            driver=adbc_driver_oracle._driver_path(),
            entrypoint="AdbcDriverOracleInit",
            uri="oracle://u@127.0.0.1:1/none",
            **{"oracle.query.batch_size": "123", "oracle.auth.token": "t"},
        )
        assert db.get_option("oracle.query.batch_size") == "123"
        db.close()
        """
    )
    assert proc.returncode == 0, proc.stderr
    assert proc.stderr.strip() == ""


def test_errors_use_the_driver_slug():
    import adbc_driver_manager

    import adbc_driver_oracle

    with pytest.raises(adbc_driver_manager.NotSupportedError, match=r"^NOT_IMPLEMENTED: \[oracle\] unknown database option"):
        adbc_driver_manager.AdbcDatabase(
            driver=adbc_driver_oracle._driver_path(),
            uri="oracle://u@127.0.0.1:1/none",
            **{"oracle.no_such_option": "x"},
        )


@pytest.mark.integration
def test_old_and_new_names_against_the_server(oracle_server):
    import adbc_driver_oracle.dbapi as oracle

    for key in ("adbc.oracle.batch_size", "oracle.query.batch_size"):
        with oracle.connect(uri=oracle_server.uri, db_kwargs={key: "4096"}) as conn, conn.cursor() as cur:
            cur.execute("SELECT LEVEL AS N FROM DUAL CONNECT BY LEVEL <= 10000")
            reader = cur.fetch_record_batch()
            sizes = [batch.num_rows for batch in reader]
            assert sum(sizes) == 10000
            assert max(sizes) == 4096, (key, sizes)
            assert conn.adbc_connection.get_option(key="adbc.oracle.batch_size") == "4096"
            assert conn.adbc_connection.get_option(key="oracle.query.batch_size") == "4096"


def test_connect_error_keeps_the_text_the_install_canary_matches():
    # gizmodata/adbc-install's weekly canary loads the driver by name and expects this
    # substring in the error for a closed port; the [oracle] prefix may come first.
    import adbc_driver_manager

    import adbc_driver_oracle

    db = adbc_driver_manager.AdbcDatabase(
        driver=adbc_driver_oracle._driver_path(), uri="oracle://user:pass@127.0.0.1:1/service"
    )
    try:
        with pytest.raises(adbc_driver_manager.Error) as excinfo:
            adbc_driver_manager.AdbcConnection(db)
    finally:
        db.close()
    message = str(excinfo.value)
    assert "oracle: cannot connect to 127.0.0.1:1" in message, message
    assert "[oracle] oracle: cannot connect to 127.0.0.1:1" in message, message


def test_log_level_variable_is_silent():
    # The driver has no logger: ADBC_DRIVER_ORACLE_LOG_LEVEL (and the Db2 name the
    # C library used to read) must not print anything when a database is created.
    for name in ("ADBC_DRIVER_ORACLE_LOG_LEVEL", "ADBC_DRIVER_DB2_LOG_LEVEL"):
        for level in ("debug", "info", "warn", "error", "bogus"):
            proc = subprocess.run(
                args=[
                    sys.executable,
                    "-c",
                    textwrap.dedent(
                        """
                        import adbc_driver_manager, adbc_driver_oracle
                        db = adbc_driver_manager.AdbcDatabase(
                            driver=adbc_driver_oracle._driver_path(), uri="oracle://u@127.0.0.1:1/none"
                        )
                        db.close()
                        """
                    ),
                ],
                capture_output=True,
                text=True,
                timeout=120,
                check=False,
                env={**os.environ, name: level},
            )
            assert proc.returncode == 0, proc.stderr
            assert proc.stderr == "", (name, level, proc.stderr)


def test_uri_read_back_hides_the_password():
    import adbc_driver_manager

    import adbc_driver_oracle

    db = adbc_driver_manager.AdbcDatabase(
        driver=adbc_driver_oracle._driver_path(), uri="oracle://scott:S3cretPW@127.0.0.1:1/FREEPDB1?password=S3cretPW"
    )
    try:
        got = db.get_option("uri")
    finally:
        db.close()
    assert "S3cretPW" not in got
    assert got == "oracle://scott:xxxxx@127.0.0.1:1/FREEPDB1?password=xxxxx"


def test_uri_errors_never_quote_the_password():
    import adbc_driver_manager

    import adbc_driver_oracle

    for uri in (
        "oracle://scott:S3cret%zz@db.example.invalid:1521/S",
        "db.example.invalid:1521/S?x=%zz&password=S3cret",
        "scott/S3cret@db.example.invalid:1521/S",
    ):
        db = adbc_driver_manager.AdbcDatabase(driver=adbc_driver_oracle._driver_path(), uri=uri)
        try:
            with pytest.raises(adbc_driver_manager.Error) as excinfo:
                adbc_driver_manager.AdbcConnection(db)
        finally:
            db.close()
        message = str(excinfo.value)
        assert "[oracle] " in message, message
        assert "S3cret" not in message, message
        assert "%zz" not in message, message


@pytest.mark.integration
def test_connection_options_before_init_new_name_wins(oracle_server):
    # Options set on a connection before Init reach the driver as one batch; with
    # both names the new one must win every time, not by Go's map order.
    import adbc_driver_manager

    import adbc_driver_oracle

    db = adbc_driver_manager.AdbcDatabase(driver=adbc_driver_oracle._driver_path(), uri=oracle_server.uri)
    try:
        for _ in range(40):
            conn = adbc_driver_manager.AdbcConnection(
                db, **{"adbc.oracle.number_mode": "string", "oracle.query.number_type": "decimal"}
            )
            try:
                assert conn.get_option("oracle.query.number_type") == "decimal"
            finally:
                conn.close()
    finally:
        db.close()
