"""
setup.py for the adbc-driver-oracle Python wheel.

The Go-built c-shared library must be copied into the package source
tree before the wheel is built. The path to the library is passed
via the ``ADBC_ORACLE_LIBRARY`` environment variable, mirroring the
adbc-driver-flightsql convention.

Lives at the repo root (not under python/) so the PyPI README can
just be ``README.md`` — no parent-directory reference or mirror dance.
"""

import os
import shutil
import sys
from pathlib import Path

from setuptools import setup
from setuptools.command.bdist_wheel import bdist_wheel
from setuptools.dist import Distribution

source_root = Path(__file__).parent
package_dir = source_root / "python" / "adbc_driver_oracle"


class BinaryDistribution(Distribution):
    """Force setuptools to build a platform-specific wheel.

    Without this, setuptools defaults to ``py3-none-any.whl`` for every
    matrix entry — and since they share a filename, only the last one
    uploaded to PyPI survives. With ``has_ext_modules`` returning True
    setuptools writes a per-platform tag and pip picks the right wheel
    per user platform.
    """

    def has_ext_modules(self):  # noqa: D401
        return True


class BinaryWheel(bdist_wheel):
    """Tag wheels as ``py3-none-<platform>`` rather than
    ``cp<ver>-cp<ver>-<platform>``.

    Our bundled c-shared library isn't a CPython extension — it's an
    arbitrary native lib loaded via ``ctypes`` / ``adbc_driver_manager``.
    Any CPython 3.10+ can load it. Without this override, setuptools
    would tag each wheel with the Python ABI of the build host
    (e.g. ``cp314-cp314-macosx_..``), which means we'd need
    5 Pythons × 4 platforms = 20 wheels per release. With it, we need
    one wheel per platform — 4 total.
    """

    def get_tag(self):  # noqa: D401
        _python, _abi, plat = bdist_wheel.get_tag(self)
        # PyPI rejects bare ``linux_x86_64`` / ``linux_aarch64`` tags
        # (only manylinux / musllinux / specific tags are accepted), so
        # rewrite to manylinux2014 — glibc 2.17, broadly compatible. Our
        # Go-cgo binary links only to libc/libpthread/libdl/librt with
        # ancient symbols, so this is honest, not optimistic. Proper
        # auditwheel-repair-in-a-container is a future improvement.
        if plat == "linux_x86_64":
            plat = "manylinux2014_x86_64"
        elif plat == "linux_aarch64":
            plat = "manylinux2014_aarch64"
        return ("py3", "none", plat)


def _library_suffix() -> str:
    if sys.platform.startswith("linux"):
        return "so"
    if sys.platform == "darwin":
        return "dylib"
    if sys.platform.startswith("win"):
        return "dll"
    return "so"


def _normalize_library_path(p: str) -> str:
    """Translate Git Bash / MSYS unix-style paths to native Windows form.

    On Windows runners (and on dev machines that build via Git Bash),
    `$PWD` is a path like ``/d/a/repo``. ``Path("/d/a/repo")`` on Windows
    rebases at the current drive root, not at the intended ``D:`` drive,
    so the copy below ends up at the wrong location. We translate
    ``/<letter>/<rest>`` to ``<letter>:/<rest>`` to dodge that.
    """
    if not sys.platform.startswith("win") or not p.startswith("/"):
        return p
    rest = p.lstrip("/")
    if len(rest) >= 2 and rest[1] == "/" and rest[0].isalpha():
        return f"{rest[0].upper()}:/{rest[2:]}"
    return p


# Bundle the c-shared library produced by `make -C pkg/oracle`.
library = os.environ.get("ADBC_ORACLE_LIBRARY")
target = package_dir / f"libadbc_driver_oracle.{_library_suffix()}"

if library:
    library = _normalize_library_path(library)
    library_path = Path(library).resolve()
    if not library_path.is_file():
        raise ValueError(
            f"ADBC_ORACLE_LIBRARY points at {library_path!r} but no file is there. "
            f"Original env value: {os.environ.get('ADBC_ORACLE_LIBRARY')!r}"
        )
    if not target.exists() or library_path != target.resolve():
        shutil.copy(library_path, target)
elif os.environ.get("_ADBC_IS_SDIST", "").strip().lower() in ("1", "true"):
    pass  # building sdist — skip the ADBC_ORACLE_LIBRARY check
elif not target.is_file():
    raise ValueError(
        "ADBC_ORACLE_LIBRARY env var is required when building a wheel "
        "(it should point to the libadbc_driver_oracle.{so,dylib,dll} "
        "produced by `make -C pkg/oracle`)."
    )


def _read_version(pkg_path: Path) -> str:
    from importlib.util import module_from_spec, spec_from_file_location

    spec = spec_from_file_location("version", pkg_path / "_version.py")
    module = module_from_spec(spec)
    spec.loader.exec_module(module)  # type: ignore[union-attr]
    return module.__version__  # type: ignore[attr-defined]


setup(
    version=_read_version(package_dir),
    distclass=BinaryDistribution,
    cmdclass={"bdist_wheel": BinaryWheel},
)
