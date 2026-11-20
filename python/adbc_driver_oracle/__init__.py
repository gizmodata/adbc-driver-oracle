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
    """Database options specific to the Oracle driver.

    The keys follow the ADBC driver style guide (``oracle.<option>``, grouped
    as ``oracle.query.*``, ``oracle.tls.*``, ``oracle.auth.*``,
    ``oracle.session.*`` and ``oracle.nne.*``). The pre-1.1.0 names
    (``adbc.oracle.*``) still work but print a one-time deprecation warning
    to stderr; they will be removed in 2.0.0. Members renamed in 1.1.0 keep
    their old member name as an alias (``NUMBER_MODE`` is ``NUMBER_TYPE``).
    """

    #: Database host (alternative to putting it in the URI).
    HOST = "oracle.host"

    #: Listener port (default 1521).
    PORT = "oracle.port"

    #: Service name, e.g. ``FREEPDB1``.
    SERVICE_NAME = "oracle.service_name"

    #: SID, as an alternative to a service name.
    SID = "oracle.sid"

    #: Set to "true" to use TLS (``tcps``) transport.
    TLS = "oracle.tls"

    #: Path to a PEM CA bundle used to verify the server certificate.
    TLS_CA_CERT = "oracle.tls.ca_cert"

    #: Set to "true" to skip server certificate verification.
    TLS_SKIP_VERIFY = "oracle.tls.skip_verify"

    #: Host name used for TLS verification / SNI (defaults to the host).
    TLS_SERVER_NAME = "oracle.tls.server_name"

    #: Directory containing ``ewallet.pem`` (Autonomous Database wallet);
    #: implies TLS.
    WALLET_LOCATION = "oracle.tls.wallet_location"

    #: Password for the wallet's private key.
    WALLET_PASSWORD = "oracle.tls.wallet_password"

    #: OAuth / IAM bearer token for token authentication (TLS only).
    TOKEN = "oracle.auth.token"

    #: Privileged connection mode: "sysdba", "sysoper", "sysasm", ...
    MODE = "oracle.auth.mode"

    #: Dial timeout in seconds (default 30; fractions and Go durations like
    #: "1.5s" are accepted too).
    CONNECT_TIMEOUT_SECONDS = "oracle.connect_timeout_seconds"
    CONNECT_TIMEOUT = "oracle.connect_timeout_seconds"  # alias, before 1.1.0

    #: Maximum rows per Arrow record batch (default 65536).
    BATCH_SIZE = "oracle.query.batch_size"

    #: Rows fetched per server round trip (default: batch size, max 65536).
    PREFETCH_ROWS = "oracle.query.prefetch_rows"

    #: NUMBER -> Arrow policy: "auto" (default), "decimal", "double", "string".
    NUMBER_TYPE = "oracle.query.number_type"
    NUMBER_MODE = "oracle.query.number_type"  # alias, before 1.1.0

    #: INTERVAL -> Arrow policy: "monthdaynano" (default), "duration", "string".
    INTERVAL_TYPE = "oracle.query.interval_type"
    INTERVAL_MODE = "oracle.query.interval_type"  # alias, before 1.1.0

    #: DATE -> Arrow policy: "timestamp" (default) or "date32".
    DATE_TYPE = "oracle.query.date_type"
    DATE_MODE = "oracle.query.date_type"  # alias, before 1.1.0

    #: Approximate maximum bytes per Arrow record batch (0 = unlimited).
    BATCH_BYTES = "oracle.query.batch_bytes"

    #: Set to "true" to disable out-of-band (TCP urgent) cancellation breaks.
    DISABLE_OOB = "oracle.disable_oob"

    #: Set to "true" to let accounts that have only a 10g (O3LOGON, DES-based)
    #: password verifier log in. Off by default: the 10g hash is weak, and a
    #: man-in-the-middle on a non-TLS connection could request it to get an
    #: offline-crackable hash of the password.
    ALLOW_10G_VERIFIER = "oracle.auth.allow_10g_verifier"

    #: Native Network Encryption / data integrity level: "accepted"
    #: (default), "requested", "required" or "rejected".
    NNE = "oracle.nne"

    #: Data-integrity (checksum) level, if different from NNE.
    NNE_CHECKSUM = "oracle.nne.checksum"

    #: Comma-separated encryption-algorithm preference (e.g. "AES256").
    NNE_ENCRYPTION_ALGORITHMS = "oracle.nne.encryption_algorithms"

    #: Comma-separated checksum-algorithm preference (e.g. "SHA512").
    NNE_CHECKSUM_ALGORITHMS = "oracle.nne.checksum_algorithms"

    #: Set to "true" to annotate JSON / object (arrow.json), SDO_GEOMETRY
    #: (geoarrow.wkb) and XMLType (arrow.opaque) columns with Arrow
    #: extension-type metadata.
    USE_EXTENSION_TYPES = "oracle.query.use_extension_types"

    #: Session TIME_ZONE (default "+00:00" so TIMESTAMP WITH LOCAL TIME
    #: ZONE values arrive as UTC).
    SESSION_TIME_ZONE = "oracle.session.time_zone"

    #: Requested session data unit size in bytes.
    SDU = "oracle.sdu"

    #: Set to "true" to log TNS packets to stderr.
    TRACE = "oracle.trace"

    #: Program name reported to the server (``V$SESSION.PROGRAM``,
    #: ``SYS_CONTEXT('USERENV', 'CLIENT_PROGRAM_NAME')``).
    APPLICATION_NAME = "oracle.session.application_name"

    #: Current schema set after connecting (``ALTER SESSION SET CURRENT_SCHEMA``).
    CURRENT_SCHEMA = "oracle.session.current_schema"


class ConnectionOptions(enum.Enum):
    """Connection options specific to the Oracle driver (settable after
    connect via ``adbc_connection.set_options``; ``NNE_ACTIVE`` and
    ``NNE_ALGORITHMS`` are read-only, via ``adbc_connection.get_option``)."""

    #: Maximum rows per Arrow record batch.
    BATCH_SIZE = "oracle.query.batch_size"

    #: Rows fetched per server round trip.
    PREFETCH_ROWS = "oracle.query.prefetch_rows"

    #: NUMBER -> Arrow policy: "auto", "decimal", "double", "string".
    NUMBER_TYPE = "oracle.query.number_type"
    NUMBER_MODE = "oracle.query.number_type"  # alias, before 1.1.0

    #: INTERVAL -> Arrow policy: "monthdaynano", "duration", "string".
    INTERVAL_TYPE = "oracle.query.interval_type"
    INTERVAL_MODE = "oracle.query.interval_type"  # alias, before 1.1.0

    #: DATE -> Arrow policy: "timestamp" or "date32".
    DATE_TYPE = "oracle.query.date_type"
    DATE_MODE = "oracle.query.date_type"  # alias, before 1.1.0

    #: Approximate maximum bytes per Arrow record batch.
    BATCH_BYTES = "oracle.query.batch_bytes"

    #: Set to "true" to annotate columns with Arrow extension-type metadata.
    USE_EXTENSION_TYPES = "oracle.query.use_extension_types"

    #: Read-only: "true" when Native Network Encryption / data integrity is
    #: active on the session.
    NNE_ACTIVE = "oracle.nne.active"

    #: Read-only: the negotiated "<encryption>,<checksum>" algorithms.
    NNE_ALGORITHMS = "oracle.nne.algorithms"

    #: End-to-end tracing attributes (``V$SESSION.MODULE`` etc.).
    MODULE = "oracle.session.module"
    ACTION = "oracle.session.action"
    CLIENT_INFO = "oracle.session.client_info"
    CLIENT_IDENTIFIER = "oracle.session.client_identifier"


class StatementOptions(enum.Enum):
    """Statement options specific to the Oracle driver (bulk ingest)."""

    #: Rows per array-bind INSERT round trip during ingest (default 5000).
    INGEST_BATCH_ROWS = "oracle.ingest.batch_rows"

    #: VARCHAR2 length used for Arrow string columns in generated DDL
    #: (default 4000).
    INGEST_VARCHAR_LENGTH = "oracle.ingest.varchar_length"

    #: RAW length used for Arrow binary columns in generated DDL (default 2000).
    INGEST_RAW_LENGTH = "oracle.ingest.raw_length"

    #: Column type for Arrow string columns: VARCHAR2 (default), NVARCHAR2, CLOB, NCLOB.
    INGEST_STRING_TYPE = "oracle.ingest.string_type"

    #: Column type for Arrow binary columns: RAW (default) or BLOB.
    INGEST_BINARY_TYPE = "oracle.ingest.binary_type"

    #: Column type for Arrow list/struct/map columns (stored as JSON text):
    #: JSON (default on 21c+, else CLOB), CLOB, VARCHAR2 or BLOB.
    INGEST_STRUCT_TYPE = "oracle.ingest.struct_type"

    #: Tablespace for tables created by ingest.
    INGEST_TABLESPACE = "oracle.ingest.tablespace"


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
        ``oracle.tls`` to enable TLS.
    """
    kwargs = {"driver": _driver_path(), "entrypoint": "AdbcDriverOracleInit", "uri": uri}
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
