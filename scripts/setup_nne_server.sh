#!/usr/bin/env bash
# Makes a gvenzl/oracle-free container's server require or reject Native
# Network Encryption, for internal/ttc/live_nne_test.go: sets
# SQLNET.ENCRYPTION_SERVER and SQLNET.CRYPTO_CHECKSUM_SERVER in its sqlnet.ora
# (read by each new connection, so no restart). Exits non-zero if any step fails.
#
#   scripts/setup_nne_server.sh <container> REQUIRED|REJECTED
#
# Local containers (CI runs the same two as services):
#   docker run -d --name oracle-nne          -p 1523:1521 -e ORACLE_PASSWORD=tiger \
#     -e APP_USER=scott -e APP_USER_PASSWORD=tiger gvenzl/oracle-free:23-slim-faststart
#   docker run -d --name oracle-nne-rejected -p 1524:1521 -e ORACLE_PASSWORD=tiger \
#     -e APP_USER=scott -e APP_USER_PASSWORD=tiger gvenzl/oracle-free:23-slim-faststart
#   scripts/setup_nne_server.sh oracle-nne REQUIRED
#   scripts/setup_nne_server.sh oracle-nne-rejected REJECTED
# Then: ADBC_ORACLE_NNE_URI=oracle://scott:tiger@127.0.0.1:1523/FREEPDB1
#       ADBC_ORACLE_NNE_REJECTED_URI=oracle://scott:tiger@127.0.0.1:1524/FREEPDB1
set -euo pipefail
container=${1:?usage: $0 <container> REQUIRED|REJECTED}
level=${2:?usage: $0 <container> REQUIRED|REJECTED}
case "$level" in
  REQUIRED | REJECTED) ;;
  *) echo "level must be REQUIRED or REJECTED, not $level" >&2; exit 2 ;;
esac

docker exec -e LEVEL="$level" "$container" bash -c '
  set -euo pipefail
  dir=$( (orabasehome 2>/dev/null || echo "$ORACLE_HOME") | tail -1)/network/admin
  file="$dir/sqlnet.ora"
  touch "$file"
  # Replace any earlier setting, so the script can run again with the other level.
  sed -i -e "/^SQLNET.ENCRYPTION_SERVER/d" -e "/^SQLNET.CRYPTO_CHECKSUM_SERVER/d" "$file"
  printf "SQLNET.ENCRYPTION_SERVER = %s\nSQLNET.CRYPTO_CHECKSUM_SERVER = %s\n" "$LEVEL" "$LEVEL" >> "$file"
  grep -E "^SQLNET.(ENCRYPTION|CRYPTO_CHECKSUM)_SERVER" "$file"
'
