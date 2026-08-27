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
