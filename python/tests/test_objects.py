"""Object types, collections, XMLType and SDO_GEOMETRY.

User-defined types and collections come back as JSON text (optionally
annotated with the ``arrow.json`` extension type), XMLType as XML text and
``MDSYS.SDO_GEOMETRY`` as GeoArrow WKB. The geometry tests need an Oracle
with Spatial installed (the ``slim`` container images omit it); point
``ADBC_ORACLE_SPATIAL_URI`` at one to run them.
"""

from __future__ import annotations

import json
import os
import uuid

import pyarrow as pa
import pytest

from .conftest import drop_table_quietly

pytestmark = pytest.mark.integration


def _connect(oracle_server, **db_kwargs):
    import adbc_driver_oracle.dbapi as oracle

    return oracle.connect(uri=oracle_server.uri, db_kwargs=db_kwargs)


def _drop_type_quietly(cur, name):
    try:
        cur.execute(f"DROP TYPE {name} FORCE")
    except Exception:
        pass


def test_object_and_collection_columns_as_json(oracle_server, table_name):
    sfx = uuid.uuid4().hex[:8].upper()
    addr, addrs, person = f"ADBC_ADDR_{sfx}", f"ADBC_ADDRS_{sfx}", f"ADBC_PERSON_{sfx}"
    with _connect(oracle_server, **{"adbc.oracle.use_extension_types": "true"}) as conn, conn.cursor() as cur:
        cur.execute(f"CREATE TYPE {addr} AS OBJECT (street VARCHAR2(40), zip NUMBER(5), since DATE)")
        cur.execute(f"CREATE TYPE {addrs} AS VARRAY(5) OF {addr}")
        cur.execute(
            f"CREATE TYPE {person} AS OBJECT (name VARCHAR2(30), age NUMBER, home {addr}, others {addrs},"
            " tags SYS.ODCIVARCHAR2LIST, score BINARY_DOUBLE, flag BOOLEAN, note CLOB)"
        )
        try:
            cur.execute(f"CREATE TABLE {table_name} (id NUMBER, p {person}, x XMLTYPE)")
            cur.execute(
                f"""INSERT INTO {table_name} VALUES (1,
                    {person}('Ann', 41, {addr}('1 Main St', 12345, DATE '2020-01-02'),
                             {addrs}({addr}('2 Side St', 54321, NULL), NULL),
                             SYS.ODCIVARCHAR2LIST('a', 'b'), 2.5, TRUE, 'a clob note'),
                    XMLTYPE('<doc><a>1</a></doc>'))"""
            )
            cur.execute(f"INSERT INTO {table_name} VALUES (2, NULL, NULL)")
            cur.execute(f"SELECT id, p, x FROM {table_name} ORDER BY id")
            table = cur.fetch_arrow_table()
            # pyarrow materialises the extension types from the field metadata
            assert str(table.schema.field("P").type) == "extension<arrow.json>"
            assert str(table.schema.field("X").type).startswith("extension<arrow.opaque")
            assert table.schema.field("P").type.storage_type == pa.string()
            rows = table.to_pylist()
            p = json.loads(rows[0]["P"])
            assert p["NAME"] == "Ann" and p["AGE"] == 41
            assert p["HOME"] == {"STREET": "1 Main St", "ZIP": 12345, "SINCE": "2020-01-02T00:00:00"}
            assert p["OTHERS"] == [{"STREET": "2 Side St", "ZIP": 54321, "SINCE": None}, None]
            assert p["TAGS"] == ["a", "b"] and p["SCORE"] == 2.5 and p["FLAG"] is True
            assert p["NOTE"] == "a clob note"
            assert "<a>1</a>" in rows[0]["X"]
            assert rows[1]["P"] is None and rows[1]["X"] is None
        finally:
            drop_table_quietly(cur, table_name)
            for t in (person, addrs, addr):
                _drop_type_quietly(cur, t)


def test_nested_arrow_types_ingest_as_json(oracle_server, table_name):
    with _connect(oracle_server) as conn, conn.cursor() as cur:
        table = pa.table(
            {
                "id": pa.array([1, 2], pa.int64()),
                "tags": pa.array([["a", "b"], None], pa.list_(pa.string())),
                "meta": pa.array([{"k": "x", "n": 7}, None], pa.struct([("k", pa.string()), ("n", pa.int32())])),
            }
        )
        try:
            cur.adbc_ingest(table_name=table_name, data=table, mode="create")
            cur.execute(f"SELECT id, tags, meta FROM {table_name} ORDER BY id")
            rows = cur.fetch_arrow_table().to_pylist()
            assert json.loads(rows[0]["TAGS"]) == ["a", "b"]
            assert json.loads(rows[0]["META"]) == {"k": "x", "n": 7}
            assert rows[1]["TAGS"] is None and rows[1]["META"] is None
        finally:
            drop_table_quietly(cur, table_name)


@pytest.fixture(scope="session")
def spatial_uri():
    uri = os.environ.get("ADBC_ORACLE_SPATIAL_URI")
    if not uri:
        pytest.skip(reason="ADBC_ORACLE_SPATIAL_URI not set (needs Oracle with Spatial)")
    return uri


def test_sdo_geometry_as_geoarrow_wkb(spatial_uri, table_name):
    import adbc_driver_oracle.dbapi as oracle

    shapely = pytest.importorskip("shapely")
    with oracle.connect(uri=spatial_uri, db_kwargs={"adbc.oracle.use_extension_types": "true"}) as conn, conn.cursor() as cur:
        cur.execute(f"CREATE TABLE {table_name} (id NUMBER, g MDSYS.SDO_GEOMETRY)")
        try:
            cur.execute(f"INSERT INTO {table_name} VALUES (1, SDO_GEOMETRY(2001, 4326, SDO_POINT_TYPE(-122.4, 37.8, NULL), NULL, NULL))")
            cur.execute(
                f"INSERT INTO {table_name} VALUES (2, SDO_GEOMETRY(2003, NULL, NULL, SDO_ELEM_INFO_ARRAY(1,1003,1),"
                " SDO_ORDINATE_ARRAY(0,0, 10,0, 10,10, 0,10, 0,0)))"
            )
            cur.execute(
                f"INSERT INTO {table_name} VALUES (3, SDO_GEOMETRY(2007, NULL, NULL, SDO_ELEM_INFO_ARRAY(1,1003,3, 5,1003,3),"
                " SDO_ORDINATE_ARRAY(0,0, 1,1, 5,5, 6,6)))"
            )
            cur.execute(f"INSERT INTO {table_name} VALUES (4, NULL)")
            cur.execute(f"SELECT id, g FROM {table_name} ORDER BY id")
            table = cur.fetch_arrow_table()
            assert table.schema.field("G").type == pa.binary()
            assert table.schema.field("G").metadata[b"ARROW:extension:name"] == b"geoarrow.wkb"
            geoms = [shapely.from_wkb(v) if v is not None else None for v in table.column("G").to_pylist()]
            assert geoms[0].wkt == "POINT (-122.4 37.8)"
            assert geoms[1].geom_type == "Polygon" and geoms[1].area == 100
            assert geoms[2].geom_type == "MultiPolygon" and geoms[2].area == 2
            assert geoms[3] is None
        finally:
            drop_table_quietly(cur, table_name)
