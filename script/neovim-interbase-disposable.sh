#!/usr/bin/env bash
# Run the real SQLs LSP in headless Neovim against an isolated InterBase fixture.
# IMAGE must already exist locally; never pull or write to a shared database.
set -Eeuo pipefail
umask 077

[[ -n "${IMAGE:-}" ]] || { echo 'Set IMAGE to an existing local InterBase 15 image ID.' >&2; exit 2; }
docker image inspect -- "$IMAGE" >/dev/null
sqls_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
root="$(mktemp -d /tmp/opencode/sqls-native-b4.XXXXXXXX)"
cid=''
cleanup() {
  local rc=$?
  trap - EXIT
  # docker may write its cidfile before `docker run --detach` returns (or times out).
  if [[ -z "$cid" && -f "$root/container.cid" ]]; then
    cid="$(cat -- "$root/container.cid")"
  fi
  if [[ ! "$cid" =~ ^[[:xdigit:]]{12,64}$ ]]; then cid=''; fi
  if [[ -n "$cid" ]]; then docker rm -f -- "$cid" >/dev/null 2>&1 || :; fi
  rm -rf -- "$root"
  exit "$rc"
}
trap cleanup EXIT

printf '%s\n' 'IB_SYSDBA_USER=SYSDBA' 'IB_SYSDBA_PASSWORD=masterkey' \
  'IB_DATA_DIR=/var/lib/interbase/data' 'IB_BACKUP_DIR=/var/lib/interbase/backups' \
  'IB_LOG_DIR=/var/lib/interbase/logs' 'IB_BACKUP_DATABASES=' > "$root/server.env"
cid="$(timeout 30s docker run --pull=never --detach --cidfile="$root/container.cid" --publish '127.0.0.1::3050' \
  --mount 'type=tmpfs,destination=/var/lib/interbase' --env-file "$root/server.env" "$IMAGE")"
[[ "$cid" =~ ^[[:xdigit:]]{12,64}$ ]] || { echo 'Invalid disposable container ID.' >&2; exit 1; }
port="$(docker port "$cid" 3050/tcp | sed 's/.*://')"
[[ "$port" =~ ^[0-9]{1,5}$ ]] || { echo 'Invalid private database port.' >&2; exit 1; }
docker cp -L "$cid:/opt/interbase/lib/libgds.so" "$root/libgds.so"
dbpath=/var/lib/interbase/data/sqls_native_acceptance.ib
cat > "$root/fixture.sql" <<EOF
SET SQL DIALECT 3;
CREATE DATABASE 'localhost/3050:$dbpath' USER 'SYSDBA' PASSWORD 'masterkey' WITH ADMIN OPTION DEFAULT CHARACTER SET UTF8;
CREATE TABLE MDE_SMOKE (ID INTEGER NOT NULL PRIMARY KEY, BODY VARCHAR(80));
INSERT INTO MDE_SMOKE (ID, BODY) VALUES (1, 'B4_NATIVE_MARKER');
INSERT INTO MDE_SMOKE (ID, BODY) VALUES (2, 'SECOND_MARKER');
COMMIT;
SELECT COUNT(*) FROM MDE_SMOKE;
QUIT;
EOF
ready=0
for ((i=1; i<=12; i++)); do
  timeout 15s docker exec -i "$cid" /opt/interbase/bin/isql -q -s 3 -names UTF8 \
    < "$root/fixture.sql" > "$root/isql.log" 2>&1 || :
  if grep -Eq '(^|[^0-9])2([^0-9]|$)' "$root/isql.log" && \
     ! grep -Eiq 'statement[[:space:]]+failed|dynamic[[:space:]]+sql[[:space:]]+error|sqlcode[[:space:]]*=[[:space:]]*-[0-9]+' "$root/isql.log"; then
    ready=1
    break
  fi
  # Never retry CREATE DATABASE after it wrote a partial file.
  if docker exec "$cid" test -f "$dbpath"; then break; fi
  sleep 1
done
if [[ "$ready" != 1 ]]; then
  echo 'Disposable fixture initialization failed (password redacted):' >&2
  sed 's/masterkey/[redacted]/g' "$root/isql.log" >&2
  exit 1
fi

cd -- "$sqls_root"
go build -tags interbase -o "$root/sqls-interbase" .
export SQLS_NATIVE_TARGET="localhost/$port:$dbpath"
export SQLS_NATIVE_USER=SYSDBA SQLS_NATIVE_PASSWORD=masterkey
export SQLS_NATIVE_BINARY="$root/sqls-interbase"
export LD_LIBRARY_PATH="$root${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
printf 'Disposable InterBase fixture: image=%s private-port=%s\n' "$IMAGE" "$port"
timeout 90s nvim --headless -u NONE -l script/neovim-interbase-smoke.lua
