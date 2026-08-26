"""ADBC driver-manifest installation.

Connection profiles (adbc-driver-manager >= 1.11) reference drivers by
name (``driver = "oracle"``) or URI scheme (``uri = "oracle://..."``).
The driver manager resolves that name by finding a ``oracle.toml``
driver manifest on its filesystem search path — there is no Python
entry-point mechanism. This module writes that manifest, pointing at
the shared library bundled in this wheel.

Manifest search paths are absolute-path only (relative ``Driver.shared``
paths resolve against the process CWD and are rejected by default), so
the manifest must be generated post-install, when the wheel's real
location is known::

    python -m adbc_driver_oracle install-manifest

writes to ``{sys.prefix}/etc/adbc/drivers/`` inside a virtualenv (the
driver manager auto-searches it) or the per-user ADBC config directory
otherwise.
"""

from __future__ import annotations

import os
import pathlib
import platform
import sys

__all__ = ["install_manifest", "manifest_dir", "render_manifest"]

_MANIFEST_NAME = "oracle.toml"


def _platform_tuple() -> str:
    """Return the ADBC manifest platform key, e.g. ``macos_arm64``."""
    if sys.platform == "darwin":
        os_name = "macos"
    elif sys.platform.startswith("win"):
        os_name = "windows"
    else:
        os_name = "linux"
    machine = platform.machine().lower()
    arch = {
        "x86_64": "amd64",
        "amd64": "amd64",
        "arm64": "arm64",
        "aarch64": "arm64",
    }.get(machine, machine)
    return f"{os_name}_{arch}"


def _user_config_dir() -> pathlib.Path:
    """Per-user ADBC driver-manifest directory for this OS."""
    if sys.platform == "darwin":
        return pathlib.Path.home() / "Library/Application Support/ADBC/Drivers"
    if sys.platform.startswith("win"):
        base = os.environ.get("LOCALAPPDATA")
        root = pathlib.Path(base) if base else pathlib.Path.home() / "AppData/Local"
        return root / "ADBC/Drivers"
    xdg = os.environ.get("XDG_CONFIG_HOME")
    root = pathlib.Path(xdg) if xdg else pathlib.Path.home() / ".config"
    return root / "adbc/drivers"


def manifest_dir(scope: str = "auto") -> pathlib.Path:
    """
    Resolve the directory the manifest should be written to.

    ``"venv"`` targets ``{sys.prefix}/etc/adbc/drivers`` (auto-searched
    by adbc-driver-manager when running inside that environment);
    ``"user"`` targets the per-user ADBC config directory; ``"auto"``
    picks ``venv`` when running in a virtualenv/conda env, else ``user``.
    """
    if scope == "auto":
        in_env = sys.prefix != getattr(sys, "base_prefix", sys.prefix) or bool(
            os.environ.get("CONDA_PREFIX")
        )
        scope = "venv" if in_env else "user"
    if scope == "venv":
        return pathlib.Path(sys.prefix) / "etc/adbc/drivers"
    if scope == "user":
        return _user_config_dir()
    raise ValueError(f"unknown scope {scope!r}; expected 'auto', 'venv', or 'user'")


def render_manifest() -> str:
    """Render the oracle.toml driver manifest for this installation."""
    from . import _driver_path, __version__

    driver_path = pathlib.Path(_driver_path()).resolve()
    # The non-default entrypoint is load-bearing: without it the driver
    # manager looks for AdbcDriverInit and name-based loading fails.
    return f"""\
manifest_version = 1
name = 'ADBC Oracle Driver - Go'
publisher = 'GizmoData'
version = '{__version__}'
license = 'MIT'
url = 'https://github.com/gizmodata/adbc-driver-oracle'

[ADBC]
version = '1.1.0'

[Driver]
entrypoint = 'OracleDriverInit'

[Driver.shared]
{_platform_tuple()} = '{driver_path}'
"""


def install_manifest(
    directory: os.PathLike | str | None = None, scope: str = "auto"
) -> pathlib.Path:
    """
    Write the ``oracle.toml`` driver manifest and return its path.

    After this, ``adbc_driver_manager.dbapi.connect(uri="oracle://...")``
    and connection profiles with ``driver = "oracle"`` resolve this
    wheel's driver without any explicit driver path.
    """
    dest = pathlib.Path(directory) if directory is not None else manifest_dir(scope)
    dest.mkdir(parents=True, exist_ok=True)
    target = dest / _MANIFEST_NAME
    target.write_text(render_manifest(), encoding="utf-8")
    return target
