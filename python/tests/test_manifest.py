"""Unit tests for the driver-manifest writer (no server needed)."""

from __future__ import annotations

import pathlib
import sys

import pytest


def _has_driver_lib() -> bool:
    import adbc_driver_oracle

    try:
        adbc_driver_oracle._driver_path()
        return True
    except FileNotFoundError:
        return False


needs_lib = pytest.mark.skipif(
    not _has_driver_lib(), reason="bundled driver shared library not built"
)


@needs_lib
def test_install_manifest_explicit_dir(tmp_path):
    import adbc_driver_oracle

    target = adbc_driver_oracle.install_manifest(directory=tmp_path)
    assert target == tmp_path / "oracle.toml"
    text = target.read_text(encoding="utf-8")
    assert "manifest_version = 1" in text
    assert "entrypoint = 'AdbcDriverOracleInit'" in text
    # Driver.shared must be an absolute path to an existing library —
    # relative paths resolve against CWD and are rejected by the
    # driver manager by default.
    lib_line = next(
        line for line in text.splitlines() if "libadbc_driver_oracle" in line
    )
    lib_path = pathlib.Path(lib_line.split("=", 1)[1].strip().strip("'"))
    assert lib_path.is_absolute()
    assert lib_path.is_file()


@needs_lib
def test_install_manifest_cli(tmp_path, capsys):
    from adbc_driver_oracle.__main__ import main

    rc = main(["install-manifest", "--dir", str(tmp_path)])
    assert rc == 0
    assert (tmp_path / "oracle.toml").is_file()
    assert str(tmp_path / "oracle.toml") in capsys.readouterr().out


def test_manifest_dir_scopes(monkeypatch, tmp_path):
    from adbc_driver_oracle import _manifest

    assert _manifest.manifest_dir("venv") == pathlib.Path(sys.prefix) / "etc/adbc/drivers"
    with pytest.raises(ValueError):
        _manifest.manifest_dir("bogus")
    if sys.platform not in ("darwin",) and not sys.platform.startswith("win"):
        monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
        assert _manifest.manifest_dir("user") == tmp_path / "adbc/drivers"


def test_option_enums_follow_the_style_guide():
    """Driver option keys are ``oracle.<...>``, never ``adbc.<...>`` (ADBC driver style guide)."""
    from adbc_driver_oracle import ConnectionOptions, DatabaseOptions, StatementOptions

    for enum_cls in (DatabaseOptions, ConnectionOptions, StatementOptions):
        for opt in enum_cls.__members__.values():
            assert opt.value.startswith("oracle."), opt
            assert not opt.value.startswith("adbc."), opt
    assert DatabaseOptions.TLS.value == "oracle.tls"
    assert DatabaseOptions.SERVICE_NAME.value == "oracle.service_name"
    assert DatabaseOptions.NUMBER_TYPE.value == "oracle.query.number_type"
    # Members renamed in 1.1.0 keep their old member names as aliases.
    assert DatabaseOptions.NUMBER_MODE is DatabaseOptions.NUMBER_TYPE
    assert DatabaseOptions.CONNECT_TIMEOUT is DatabaseOptions.CONNECT_TIMEOUT_SECONDS
    assert ConnectionOptions.DATE_MODE is ConnectionOptions.DATE_TYPE
    assert StatementOptions.INGEST_STRING_TYPE.value == "oracle.ingest.string_type"
