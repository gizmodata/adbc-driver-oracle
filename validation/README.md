# ADBC driver validation suite

Runs the [adbc-drivers.org validation suite](https://github.com/adbc-drivers/validation)
(the conformance tests the ADBC Driver Foundry's drivers use) against this driver's
c-shared library and a real Oracle Database. The suite is a pinned test dependency
(`requirements.txt`); nothing here is part of the driver or the wheel. CI runs it on every
push and pull request, and a release can't publish unless it passes.

## Run it

```bash
# Oracle 23ai Free (the image CI uses); any free port works
docker run --name oracle-adbc-validation -d -p 127.0.0.1:1521:1521 \
  -e ORACLE_PASSWORD=tiger -e APP_USER=scott -e APP_USER_PASSWORD=tiger \
  gvenzl/oracle-free:23-slim-faststart

make -C pkg/oracle                      # build the driver library

uv venv --python 3.14 .venv-validation  # the suite needs Python 3.13+
VIRTUAL_ENV=$PWD/.venv-validation uv pip install -r validation/requirements.txt

export ADBC_ORACLE_TEST_URI=oracle://scott:tiger@127.0.0.1:1521/FREEPDB1
export ADBC_ORACLE_TEST_ADMIN_URI=oracle://system:tiger@127.0.0.1:1521/FREEPDB1
.venv-validation/bin/pytest -rfEsxX validation/tests/
```

- `ADBC_ORACLE_TEST_URI`: the session the suite tests (its user is the current schema).
- `ADBC_ORACLE_TEST_ADMIN_URI`: a DBA login on the same PDB. Before the tests, it creates
  the schema `ADBC_VALIDATION_SECONDARY` and grants the test user the `ANY TABLE`
  privileges the schema-qualified cases need. Use a throwaway database.
- `ADBC_ORACLE_LIBRARY`: the driver library to test (default `pkg/oracle/libadbc_driver_oracle.*`).

The run fails, never skips, when a setting is missing or the server can't be reached, and
a run in which no test passed fails too.

## What is adapted for Oracle

`tests/oracle.py` declares the features the driver implements; `queries/oracle/` overrides
the suite's cases where Oracle's SQL or types differ. Every override states the Oracle SQL
and expects what the driver's documented type mapping (README "Type mapping") produces,
with the reason recorded as a caveat in the case. The adaptations:

- **Identifier case.** The suite writes table and column names unquoted in lower case and
  expects them back verbatim; Oracle stores unquoted names upper-cased, and so does the
  driver's ingest DDL. `conftest.py` accepts an upper-case column only where the expected
  name is one Oracle folds; any other name difference fails. The GetObjects tests create
  their tables with quoted lower-case names, so those names are checked exactly.
- **Types Oracle doesn't have.** No `TIME` (those cases skip), no `SMALLINT`/`INTEGER`/
  `BIGINT` (`NUMBER(p)`: int64 up to 18 digits, decimal128(19, 0) for `NUMBER(19)`), `DATE`
  carries a time of day (timestamp[s]), `''` is NULL.
- **Temporary tables** are `GLOBAL TEMPORARY`: the definition outlives the session, only
  the rows are private. `test_temporary` checks that instead of "the table is gone".
- **Long values** go into LOB columns through the driver's `adbc.oracle.ingest.string_type`
  / `binary_type` options (in-row `VARCHAR2`/`RAW` stop at 4000/2000 bytes).
- **Empty BLOB.** An empty, non-NULL BLOB reads back as NULL: LOBs are fetched inline,
  which doesn't distinguish the two (python-oracledb's `fetch_lobs=False` behaves the same).
- **Not implemented** (skipped by the suite): `GetStatistics`, and a second catalog (an
  Oracle session is attached to one database).

Files copied or adapted from adbc-drivers projects keep their Apache-2.0 headers (see
`NOTICE`).
