"""Native Network Encryption (Oracle Advanced Networking).

Point ``ADBC_ORACLE_NNE_URI`` at an Oracle whose listener requires NNE
(``SQLNET.ENCRYPTION_SERVER=REQUIRED`` and/or
``SQLNET.CRYPTO_CHECKSUM_SERVER=REQUIRED``) to run these; otherwise they
skip. The default (slim) CI container does not require NNE.
"""

from __future__ import annotations

import os
import socket

import pytest

pytestmark = pytest.mark.integration


def _nne_uri():
    uri = os.environ.get("ADBC_ORACLE_NNE_URI")
    if not uri:
        pytest.skip(reason="ADBC_ORACLE_NNE_URI not set (needs an NNE-required Oracle)")
    return uri


def test_connect_and_query_over_nne():
    import adbc_driver_oracle.dbapi as oracle

    with oracle.connect(uri=_nne_uri()) as conn, conn.cursor() as cur:
        # A large streamed result exercises many encrypted/checksummed packets.
        cur.execute("SELECT LEVEL AS n, RPAD('x', 300, 'x') AS s FROM DUAL CONNECT BY LEVEL <= 20000")
        table = cur.fetch_arrow_table()
        assert table.num_rows == 20000
        assert table.column("N")[-1].as_py() == 20000
        # An error then reuse exercises the marker/reset keystream re-init.
        with pytest.raises(Exception, match="ORA-00942"):
            cur.execute("SELECT * FROM no_such_table_over_nne")
        cur.execute("SELECT 1 FROM DUAL")
        assert cur.fetchone() == (1,)


def test_required_server_without_nne_gives_clear_error():
    """Turning NNE off against a required server fails with guidance."""
    import adbc_driver_oracle.dbapi as oracle

    with pytest.raises(Exception, match="Native Network Encryption|ORA-12660"):
        with oracle.connect(uri=_nne_uri(), db_kwargs={"adbc.oracle.nne": "rejected"}) as conn, conn.cursor() as cur:
            cur.execute("SELECT 1 FROM DUAL")


def test_client_required_encrypts_against_accepted_server(oracle_server):
    """nne=required against a server that does not itself require NNE
    (the standard test container) must still negotiate encryption — never
    silently proceed in cleartext — and report it via nne_active."""
    import adbc_driver_oracle.dbapi as oracle

    with oracle.connect(
        uri=oracle_server.uri, db_kwargs={"adbc.oracle.nne": "required"}
    ) as conn, conn.cursor() as cur:
        assert conn.adbc_connection.get_option(key="adbc.oracle.nne_active") == "true"
        assert "AES" in conn.adbc_connection.get_option(key="adbc.oracle.nne_algorithms")
        cur.execute("SELECT LEVEL AS n FROM DUAL CONNECT BY LEVEL <= 5000")
        assert cur.fetch_arrow_table().num_rows == 5000


def test_nne_introspection_default(oracle_server):
    """The default (accepted) stays unencrypted against a non-NNE server
    and the read-only options report that truthfully."""
    import adbc_driver_oracle.dbapi as oracle

    with oracle.connect(uri=oracle_server.uri) as conn:
        assert conn.adbc_connection.get_option(key="adbc.oracle.nne_active") == "false"
        assert conn.adbc_connection.get_option(key="adbc.oracle.nne_algorithms") == ""


def test_wire_bytes_are_encrypted_with_required(oracle_server):
    """Definitive check of the fail-closed contract at the byte level: run
    the same query through a capturing TCP proxy with and without
    nne=required. The SQL text must appear on the wire in cleartext for the
    default (accepted vs. a non-NNE server) and must NOT with required."""
    import threading

    import adbc_driver_oracle.dbapi as oracle

    marker = "SELECT 8675309 FROM DUAL"

    def run_via_proxy(db_kwargs):
        captured = bytearray()
        lock = threading.Lock()
        listener = socket.create_server(("127.0.0.1", 0))
        proxy_port = listener.getsockname()[1]

        def pump(src, dst):
            try:
                while True:
                    data = src.recv(65536)
                    if not data:
                        break
                    with lock:
                        captured.extend(data)
                    dst.sendall(data)
            except OSError:
                pass
            finally:
                try:
                    dst.shutdown(socket.SHUT_WR)
                except OSError:
                    pass

        def serve():
            while True:
                try:
                    client, _ = listener.accept()
                except OSError:
                    return
                upstream = socket.create_connection((oracle_server.host, oracle_server.port))
                threading.Thread(target=pump, args=(client, upstream), daemon=True).start()
                threading.Thread(target=pump, args=(upstream, client), daemon=True).start()

        threading.Thread(target=serve, daemon=True).start()
        try:
            with oracle.connect(
                uri=f"oracle://127.0.0.1:{proxy_port}/FREEPDB1",
                db_kwargs={**oracle_server.db_kwargs, **db_kwargs},
            ) as conn, conn.cursor() as cur:
                cur.execute(marker)
                assert cur.fetchone() == (8675309,)
        finally:
            listener.close()
        with lock:
            return bytes(captured)

    cleartext = run_via_proxy(db_kwargs={})
    assert marker.encode() in cleartext, "sanity: default session should be cleartext here"

    encrypted = run_via_proxy(db_kwargs={"adbc.oracle.nne": "required"})
    assert marker.encode() not in encrypted, "required session leaked SQL text on the wire"
    assert b"8675309" not in encrypted, "required session leaked result data on the wire"


def test_required_fails_closed_against_rejecting_server():
    """nne=required against a server with SQLNET.ENCRYPTION_SERVER=REJECTED
    must raise (fail closed) rather than transmit cleartext. Needs
    ADBC_ORACLE_NNE_REJECTED_URI (see the container recipe in CLAUDE.md)."""
    import adbc_driver_oracle.dbapi as oracle

    uri = os.environ.get("ADBC_ORACLE_NNE_REJECTED_URI")
    if not uri:
        pytest.skip(reason="ADBC_ORACLE_NNE_REJECTED_URI not set (needs an NNE-rejecting Oracle)")
    with pytest.raises(Exception, match="ORA-12660"):
        with oracle.connect(uri=uri, db_kwargs={"adbc.oracle.nne": "required"}):
            pass


def test_nne_algorithm_selection():
    import adbc_driver_oracle.dbapi as oracle

    with oracle.connect(
        uri=_nne_uri(),
        db_kwargs={
            "adbc.oracle.nne": "required",
            "adbc.oracle.nne_encryption_algorithms": "AES256",
            "adbc.oracle.nne_checksum_algorithms": "SHA512",
        },
    ) as conn, conn.cursor() as cur:
        cur.execute("SELECT 42 FROM DUAL")
        assert cur.fetchone() == (42,)
