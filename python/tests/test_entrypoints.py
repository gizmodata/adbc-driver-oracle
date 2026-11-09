"""The bundled library's entrypoints, per the ADBC shared-library requirements
(https://adbc-drivers.org/building-drivers/shared-library-requirements.html).

The driver manager derives ``AdbcDriverOracleInit`` from the file name
``libadbc_driver_oracle.*`` and falls back to ``AdbcDriverInit``; both must load
the driver. ``OracleDriverInit`` is the deprecated alias releases up to 1.0.3
documented: manifests and code written for them must keep working until 2.0.0.
Everything else the Go wrapper defines (``OracleDatabaseNew`` and the like) is
hidden. No database is needed: creating an ``AdbcDatabase`` loads the driver
through the entrypoint and does not connect.
"""

from __future__ import annotations

import subprocess
import sys
import textwrap

import pytest

import adbc_driver_manager


def _driver_path() -> str:
    import adbc_driver_oracle

    try:
        return adbc_driver_oracle._driver_path()
    except FileNotFoundError:
        pytest.fail("bundled driver shared library not built")


def _open(**kwargs) -> None:
    db = adbc_driver_manager.AdbcDatabase(
        driver=_driver_path(), uri="oracle://u:p@127.0.0.1:1/none", **kwargs
    )
    db.close()


@pytest.mark.parametrize(
    "entrypoint",
    ["AdbcDriverOracleInit", "AdbcDriverInit", "OracleDriverInit"],
)
def test_entrypoint_loads(entrypoint):
    _open(entrypoint=entrypoint)


def test_default_entrypoint_from_file_name():
    # No entrypoint given: the manager tries AdbcDriverOracleInit (from the file
    # name), then AdbcDriverInit.
    _open()


def test_go_wrapper_symbols_are_hidden():
    # The positive twins above make the same call. OracleDatabaseNew exists in the
    # Go wrapper (//export) but must not be exported: 1.0.3 exported it, the
    # driver manager then called it as an init function and the process crashed,
    # so this runs in a child process.
    code = textwrap.dedent(
        """
        import sys
        import adbc_driver_manager
        try:
            adbc_driver_manager.AdbcDatabase(
                driver=sys.argv[1], entrypoint="OracleDatabaseNew",
                uri="oracle://u:p@127.0.0.1:1/none",
            ).close()
        except adbc_driver_manager.Error as e:
            print(str(e))
        else:
            print("LOADED")
        """
    )
    result = subprocess.run(
        args=[sys.executable, "-c", code, _driver_path()],
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert result.returncode == 0, result.stderr[-2000:]
    assert "LOADED" not in result.stdout
    assert "OracleDatabaseNew" in result.stdout, result.stdout
