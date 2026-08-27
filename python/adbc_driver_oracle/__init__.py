"""Apache Arrow ADBC driver for Oracle Database (pure Go, no Oracle Client)."""

from __future__ import annotations

import enum
import functools
import typing

import adbc_driver_manager

from ._version import __version__  # noqa: F401

__all__ = [
    "ConnectionOptions",
    "DatabaseOptions",
    "StatementOptions",
    "connect",
    "install_manifest",
]


class DatabaseOptions(enum.Enum):
    """Database options specific to the Oracle driver."""

    #: Database host (alternative to putting it in the URI).
    HOST = "adbc.oracle.host"

    #: Listener port (default 1521).
    PORT = "adbc.oracle.port"

    #: Service name, e.g. ``FREEPDB1``.
    SERVICE_NAME = "adbc.oracle.service_name"

    #: SID, as an alternative to a service name.
    SID = "adbc.oracle.sid"

    #: Set to "true" to use TLS (``tcps``) transport.
    TLS = "adbc.oracle.tls"

    #: Path to a PEM CA bundle used to verify the server certificate.
    TLS_CA_CERT = "adbc.oracle.tls.ca_cert"

    #: Set to "true" to skip server certificate verification.
    TLS_SKIP_VERIFY = "adbc.oracle.tls.skip_verify"

    #: Host name used for TLS verification / SNI (defaults to the host).
    TLS_SERVER_NAME = "adbc.oracle.tls.server_name"

    #: Directory containing ``ewallet.pem`` (Autonomous Database wallet);
    #: implies TLS.
    WALLET_LOCATION = "adbc.oracle.wallet_location"

    #: Password for the wallet's private key.
    WALLET_PASSWORD = "adbc.oracle.wallet_password"

    #: OAuth / IAM bearer token for token authentication (TLS only).
    TOKEN = "adbc.oracle.token"

    #: Privileged connection mode: "sysdba", "sysoper", "sysasm", ...
    MODE = "adbc.oracle.mode"

    #: Dial timeout: seconds, or a Go duration like "1.5s" (default 30).
    CONNECT_TIMEOUT = "adbc.oracle.connect_timeout"

    #: Maximum rows per Arrow record batch (default 65536).
    BATCH_SIZE = "adbc.oracle.batch_size"

    #: Rows fetched per server round trip (default: batch size, max 65536).
    PREFETCH_ROWS = "adbc.oracle.prefetch_rows"

    #: NUMBER -> Arrow policy: "auto" (default), "decimal", "double", "string".
    NUMBER_MODE = "adbc.oracle.number_mode"

    #: INTERVAL -> Arrow policy: "monthdaynano" (default), "duration", "string".
    INTERVAL_MODE = "adbc.oracle.interval_mode"

    #: DATE -> Arrow policy: "timestamp" (default) or "date32".
    DATE_MODE = "adbc.oracle.date_mode"

    #: Approximate maximum bytes per Arrow record batch (0 = unlimited).
    BATCH_BYTES = "adbc.oracle.batch_bytes"

    #: Set to "true" to disable out-of-band (TCP urgent) cancellation breaks.
    DISABLE_OOB = "adbc.oracle.disable_oob"

    #: Set to "true" to annotate JSON / object (arrow.json), SDO_GEOMETRY
    #: (geoarrow.wkb) and XMLType (arrow.opaque) columns with Arrow
    #: extension-type metadata.
    USE_EXTENSION_TYPES = "adbc.oracle.use_extension_types"

    #: Session TIME_ZONE (default "+00:00" so TIMESTAMP WITH LOCAL TIME
    #: ZONE values arrive as UTC).
    SESSION_TIME_ZONE = "adbc.oracle.session_time_zone"

    #: Requested session data unit size in bytes.
    SDU = "adbc.oracle.sdu"

    #: Set to "true" to log TNS packets to stderr.
    TRACE = "adbc.oracle.trace"

    #: Program name reported to the server (``V$SESSION.PROGRAM``,
    #: ``SYS_CONTEXT('USERENV', 'CLIENT_PROGRAM_NAME')``).
    APPLICATION_NAME = "adbc.oracle.application_name"

    #: Current schema set after connecting (``ALTER SESSION SET CURRENT_SCHEMA``).
    CURRENT_SCHEMA = "adbc.oracle.current_schema"


class ConnectionOptions(enum.Enum):
    """Connection options specific to the Oracle driver (settable after
    connect via ``adbc_connection.set_options``)."""

    #: Maximum rows per Arrow record batch.
    BATCH_SIZE = "adbc.oracle.batch_size"

    #: Rows fetched per server round trip.
    PREFETCH_ROWS = "adbc.oracle.prefetch_rows"

    #: NUMBER -> Arrow policy: "auto", "decimal", "double", "string".
    NUMBER_MODE = "adbc.oracle.number_mode"

    #: INTERVAL -> Arrow policy: "monthdaynano", "duration", "string".
    INTERVAL_MODE = "adbc.oracle.interval_mode"

    #: DATE -> Arrow policy: "timestamp" or "date32".
    DATE_MODE = "adbc.oracle.date_mode"

    #: Approximate maximum bytes per Arrow record batch.
    BATCH_BYTES = "adbc.oracle.batch_bytes"

    #: End-to-end tracing attributes (``V$SESSION.MODULE`` etc.).
    MODULE = "adbc.oracle.module"
    ACTION = "adbc.oracle.action"
    CLIENT_INFO = "adbc.oracle.client_info"
    CLIENT_IDENTIFIER = "adbc.oracle.client_identifier"


class StatementOptions(enum.Enum):
    """Statement options specific to the Oracle driver (bulk ingest)."""

    #: Rows per array-bind INSERT round trip during ingest (default 5000).
    INGEST_BATCH_ROWS = "adbc.oracle.ingest.batch_rows"

    #: VARCHAR2 length used for Arrow string columns in generated DDL
    #: (default 4000).
    INGEST_VARCHAR_LENGTH = "adbc.oracle.ingest.varchar_length"

    #: RAW length used for Arrow binary columns in generated DDL (default 2000).
    INGEST_RAW_LENGTH = "adbc.oracle.ingest.raw_length"

    #: Column type for Arrow string columns: VARCHAR2 (default), NVARCHAR2, CLOB, NCLOB.
    INGEST_STRING_TYPE = "adbc.oracle.ingest.string_type"

    #: Column type for Arrow binary columns: RAW (default) or BLOB.
    INGEST_BINARY_TYPE = "adbc.oracle.ingest.binary_type"

    #: Column type for Arrow list/struct/map columns (stored as JSON text):
    #: JSON (default on 21c+, else CLOB), CLOB, VARCHAR2 or BLOB.
    INGEST_STRUCT_TYPE = "adbc.oracle.ingest.struct_type"

    #: Tablespace for tables created by ingest.
    INGEST_TABLESPACE = "adbc.oracle.ingest.tablespace"


@functools.lru_cache(maxsize=1)
def _driver_path() -> str:
    """Resolve the path to the bundled c-shared driver library."""
    try:
        from importlib import resources
    except ImportError:  # pragma: no cover
        import importlib_resources as resources  # type: ignore[import-not-found]

    import os
    import sys

    if sys.platform == "darwin":
        candidates = ("libadbc_driver_oracle.dylib",)
    elif sys.platform.startswith("win"):
        candidates = ("libadbc_driver_oracle.dll", "adbc_driver_oracle.dll")
    else:
        candidates = ("libadbc_driver_oracle.so",)

    # resources.as_file() does NOT verify that the file exists on disk
    # (it only handles extraction from zip-backed resources), so we have
    # to check explicitly. Otherwise a stale stringified path is fed to
    # ADBC's driver manager, which on Windows misparses the drive-letter
    # colon as a `name:entrypoint` separator and reports the cryptic
    # "Could not load `D`" error.
    pkg = resources.files("adbc_driver_oracle")
    tried: list[str] = []
    for name in candidates:
        try:
            with resources.as_file(pkg / name) as path:
                resolved = str(path)
                if os.path.isfile(resolved):
                    return resolved
                tried.append(resolved)
        except (FileNotFoundError, ModuleNotFoundError):
            tried.append(name)

    raise FileNotFoundError(
        "Could not locate the bundled Oracle ADBC driver shared library. "
        "Rebuild the wheel with ADBC_ORACLE_LIBRARY pointing at "
        "libadbc_driver_oracle.{so,dylib,dll}. Looked for: "
        + ", ".join(tried)
    )


def connect(
    uri: str,
    db_kwargs: typing.Mapping[str, str] | None = None,
) -> adbc_driver_manager.AdbcDatabase:
    """
    Open a low-level ADBC database against an Oracle Database.

    Most users will want :func:`adbc_driver_oracle.dbapi.connect` instead,
    which returns a DBAPI 2.0 :class:`Connection`.

    Parameters
    ----------
    uri:
        Oracle connection URL, e.g.
        ``"oracle://scott:tiger@localhost:1521/FREEPDB1"``.
    db_kwargs:
        Optional mapping of ADBC database options. Common keys:
        ``username`` / ``password`` (instead of embedding them in the URI),
        ``adbc.oracle.tls`` to enable TLS.
    """
    kwargs = {"driver": _driver_path(), "entrypoint": "OracleDriverInit", "uri": uri}
    if db_kwargs:
        # dict.update is a C-extension method whose kwargs become new
        # dict keys, not a merge target — so the merge arg stays positional.
        kwargs.update(db_kwargs)
    return adbc_driver_manager.AdbcDatabase(**kwargs)


def install_manifest(*args, **kwargs):
    """Write the oracle.toml ADBC driver manifest; see :mod:`._manifest`.

    After installing the manifest, this driver can be resolved by name —
    via connection profiles (``driver = "oracle"``) or directly by URI
    scheme: ``adbc_driver_manager.dbapi.connect(uri="oracle://...")``.
    Also available as ``python -m adbc_driver_oracle install-manifest``.
    """
    from ._manifest import install_manifest as _impl

    return _impl(*args, **kwargs)
