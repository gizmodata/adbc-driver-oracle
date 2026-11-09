#!/usr/bin/env bash
# Prepares an Oracle container for driver/oracle/verifier_live_test.go: lets 10g
# and 11g verifiers log in (SQLNET.ALLOWED_LOGON_VERSION_SERVER=8, read by each
# new connection, so no restart) and creates ADBC_TEST_10G / _11G / _12C, each
# with only that password verifier (scripts/verifier_users.sql). Exits non-zero
# if any step fails.
#
#   scripts/setup_verifier_users.sh <container> [pdb]    # pdb defaults to XEPDB1
#
# Use a gvenzl/oracle-xe:21 container: 23ai and later refuse 10g verifiers.
# Then: ADBC_ORACLE_TEST_10G_URI=oracle://adbc_test_10g:Tiger10g@HOST:PORT/XEPDB1
#       ADBC_ORACLE_TEST_11G_URI=oracle://adbc_test_11g:Tiger11g@HOST:PORT/XEPDB1
#       ADBC_ORACLE_TEST_12C_URI=oracle://adbc_test_12c:Tiger12c@HOST:PORT/XEPDB1
set -euo pipefail
container=${1:?usage: $0 <container> [pdb]}
pdb=${2:-XEPDB1}
here=$(cd "$(dirname "$0")" && pwd)

docker exec "$container" bash -c '
  set -euo pipefail
  dir=$( (orabasehome 2>/dev/null || echo "$ORACLE_HOME") | tail -1)/network/admin
  if ! grep -q "^SQLNET.ALLOWED_LOGON_VERSION_SERVER" "$dir/sqlnet.ora" 2>/dev/null; then
    echo "SQLNET.ALLOWED_LOGON_VERSION_SERVER=8" >> "$dir/sqlnet.ora"
  fi
  grep "^SQLNET.ALLOWED_LOGON_VERSION_SERVER" "$dir/sqlnet.ora"
'
{
  echo "whenever sqlerror exit failure"
  echo "alter session set container=${pdb};"
  cat "$here/verifier_users.sql"
} | docker exec -i "$container" bash -c 'sqlplus -s -L / as sysdba'
