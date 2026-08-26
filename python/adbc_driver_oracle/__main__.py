"""Command-line helpers: ``python -m adbc_driver_oracle install-manifest``."""

from __future__ import annotations

import argparse
import sys


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="python -m adbc_driver_oracle")
    sub = parser.add_subparsers(dest="command", required=True)

    install = sub.add_parser(
        "install-manifest",
        help=(
            "Write the oracle.toml ADBC driver manifest so connection "
            "profiles and oracle:// URIs resolve this driver by name."
        ),
    )
    scope = install.add_mutually_exclusive_group()
    scope.add_argument(
        "--venv",
        action="store_const",
        dest="scope",
        const="venv",
        help="install into {sys.prefix}/etc/adbc/drivers (this environment only)",
    )
    scope.add_argument(
        "--user",
        action="store_const",
        dest="scope",
        const="user",
        help="install into the per-user ADBC config directory",
    )
    install.add_argument(
        "--dir", default=None, help="explicit destination directory (overrides scope)"
    )
    install.set_defaults(scope="auto")

    args = parser.parse_args(argv)
    if args.command == "install-manifest":
        from ._manifest import install_manifest

        path = install_manifest(directory=args.dir, scope=args.scope)
        print(f"Wrote ADBC driver manifest: {path}")
        return 0
    return 2


if __name__ == "__main__":
    sys.exit(main())
