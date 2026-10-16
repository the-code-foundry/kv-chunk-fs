#!/bin/bash
# FUSE integration test: mounts bin/kv-chunk-fs on a scratch directory and runs
# the file operations llama-server, the kv-cache proxy and clean.sh rely on.
# Needs /dev/fuse and fusermount3 on the host; leaves nothing behind.
#
#   ./test-fs.sh
set -uo pipefail
cd "$(dirname "$0")"
BIN="$PWD/bin/kv-chunk-fs"
T="$(mktemp -d -p "${TEST_DIR:-/tmp}" kvfs-test.XXXXXX)"
B="$T/backing" M="$T/mnt"
mkdir -p "$B" "$M"
pass=0 fail=0
ok()  { pass=$((pass + 1)); printf '  ok    %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  FAIL  %s\n' "$1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

mount_fs() {
  "$BIN" mount --backing "$B" "$M" >> "$T/mount.log" 2>&1 &
  MPID=$!
  for _ in $(seq 50); do mountpoint -q "$M" && return 0; sleep 0.1; done
  echo "mount failed:"; cat "$T/mount.log"; exit 1
}
umount_fs() { fusermount3 -u "$M"; for _ in $(seq 50); do mountpoint -q "$M" || return 0; sleep 0.1; done; }
cleanup() { mountpoint -q "$M" && fusermount3 -u "$M"; rm -rf "$T"; }
trap cleanup EXIT
# new chunks of the last write of $1 (from its "event=write ... file=<dir>/$1 ... new_chunks=N" line)
saved_new() { grep "event=write" "$T/mount.log" | grep "file=[^ ]*/$1 " | tail -1 | sed -E 's/.* new_chunks=([0-9]+).*/\1/'; }

head -c $((40 << 20)) /dev/urandom > "$T/a.src"           # 40 MiB of random data
{ head -c $((30 << 20)) "$T/a.src"; head -c $((2 << 20)) /dev/urandom; tail -c $((10 << 20)) "$T/a.src"; } > "$T/b.src"

mount_fs
echo "files"
mkdir -p "$M/model"
check "mkdir" '[ -d "$M/model" ]'
cp "$T/a.src" "$M/model/a.bin"
check "chunked file reads back identical" 'cmp -s "$T/a.src" "$M/model/a.bin"'
check "logical size in stat" '[ "$(stat -c %s "$M/model/a.bin")" = "$(stat -c %s "$T/a.src")" ]'
check "stored as a stub in the tree" '[ "$(stat -c %s "$B/tree/model/a.bin")" -lt 100000 ]'
check "find -printf %s sees the logical size" '[ "$(find "$M/model" -name a.bin -printf %s)" = "$((40 << 20))" ]'
echo '{"x":1}' > "$M/model/a.bin.manifest.json"
check "plain file is a plain file in the tree" 'cmp -s "$M/model/a.bin.manifest.json" "$B/tree/model/a.bin.manifest.json"'

echo "dedup"
cp "$T/a.src" "$M/model/copy.bin"
check "identical content stores no new chunks" '[ "$(saved_new copy.bin)" = 0 ]'
cp "$T/b.src" "$M/model/b.bin"
n="$(saved_new b.bin)"
check "a 2 MiB insertion stores few new chunks ($n)" '[ "${n:-999}" -le 80 ]'
check "b reads back identical" 'cmp -s "$T/b.src" "$M/model/b.bin"'

echo "proxy pattern: write .tmp, rename over the target, touch, unlink"
cp "$T/b.src" "$M/model/a.bin.tmp"
mv "$M/model/a.bin.tmp" "$M/model/a.bin"
check "rename over an existing file" 'cmp -s "$T/b.src" "$M/model/a.bin" && [ ! -e "$M/model/a.bin.tmp" ]'
touch -d "2026-01-02 03:04:05" "$M/model/a.bin"
check "utimens sets mtime" '[ "$(stat -c %Y "$M/model/a.bin")" = "$(date -d "2026-01-02 03:04:05" +%s)" ]'
exec 7< "$M/model/copy.bin"
rm "$M/model/copy.bin"
check "unlink" '[ ! -e "$M/model/copy.bin" ]'
check "an open file stays readable after unlink" '[ "$(head -c 1048576 <&7 | md5sum)" = "$(head -c 1048576 "$T/a.src" | md5sum)" ]'
exec 7<&-

echo "rewrites"
cp "$T/a.src" "$M/model/r.bin"; cp "$T/b.src" "$M/model/r.bin"
check "overwrite an existing chunked file (O_TRUNC)" 'cmp -s "$T/b.src" "$M/model/r.bin"'
python3 - "$M/model/r.bin" "$T/a.src" <<'PY' && ok "two-pass save like llama-server (truncate, then reopen O_APPEND)" || bad "two-pass save like llama-server (truncate, then reopen O_APPEND)"
import sys
src = open(sys.argv[2], "rb").read()
with open(sys.argv[1], "wb") as f: f.write(src[:20 << 20])
with open(sys.argv[1], "ab") as f: f.write(src[20 << 20:])
assert open(sys.argv[1], "rb").read() == src
PY
check "  ... both passes streamed (no staging): $(grep -c 'event=write.*file=[^ ]*/r.bin ' "$T/mount.log") writes" '! grep -q "staging copy" "$T/mount.log"'
python3 - "$M/model/r.bin" <<'PY' && ok "truncate by path while a writer is open" || bad "truncate by path while a writer is open"
import os, sys
with open(sys.argv[1], "r+b") as f:
    os.truncate(sys.argv[1], 0)
    f.seek(0); f.write(b"z" * 5000)
assert open(sys.argv[1], "rb").read() == b"z" * 5000
PY
rm "$M/model/r.bin"

echo "other writes"
cp "$T/a.src" "$M/model/c.bin"; cat "$T/a.src" | head -c 1000 >> "$M/model/c.bin"
check "append" '[ "$(stat -c %s "$M/model/c.bin")" = "$(((40 << 20) + 1000))" ] && cmp -s <(cat "$T/a.src"; head -c 1000 "$T/a.src") "$M/model/c.bin"'
check "append streams onto the existing chunks (1 new chunk: $(saved_new c.bin))" '[ "$(saved_new c.bin)" = 1 ]'
printf 'XYZ' | dd of="$M/model/c.bin" bs=1 seek=12345 conv=notrunc status=none
check "write in the middle (staging path)" '[ "$(dd if="$M/model/c.bin" bs=1 skip=12345 count=3 status=none)" = XYZ ] && [ "$(stat -c %s "$M/model/c.bin")" = "$(((40 << 20) + 1000))" ]'
truncate -s 5000000 "$M/model/c.bin"
check "truncate smaller" '[ "$(stat -c %s "$M/model/c.bin")" = 5000000 ] && cmp -s <(head -c 5000000 "$M/model/c.bin" | tail -c 100) <(head -c 5000000 "$T/a.src" | tail -c 100)'
truncate -s 0 "$M/model/c.bin"
check "truncate to zero" '[ "$(stat -c %s "$M/model/c.bin")" = 0 ] && [ "$(wc -c < "$M/model/c.bin")" = 0 ]'
python3 - "$M/model/d.bin" <<'EOF' && ok "read on a write handle, then keep writing" || bad "read on a write handle, then keep writing"
import sys
with open(sys.argv[1], "w+b") as f:
    f.write(b"a" * 300000); f.seek(1000); assert f.read(10) == b"a" * 10
    f.seek(0, 2); f.write(b"b" * 1000)
with open(sys.argv[1], "rb") as f:
    d = f.read(); assert d == b"a" * 300000 + b"b" * 1000, len(d)
EOF
: > "$M/model/e.bin"
check "empty chunked file" '[ -f "$M/model/e.bin" ] && [ "$(stat -c %s "$M/model/e.bin")" = 0 ]'
mkdir "$M/tmpdir" && rmdir "$M/tmpdir"
check "rmdir" '[ ! -e "$M/tmpdir" ]'

echo "persistence"
umount_fs; mount_fs
check "content survives a remount" 'cmp -s "$T/b.src" "$M/model/a.bin" && cmp -s "$T/b.src" "$M/model/b.bin"'
check "listing after remount" '[ "$(ls "$M/model" | tr "\n" " ")" = "a.bin a.bin.manifest.json b.bin c.bin d.bin e.bin " ]'
check "no staging leftovers" '[ -z "$(ls -A "$B/staging")" ]'

echo "status file"
check "first line is healthy" '[ "$(head -1 "$M/status")" = healthy ]'
check "listed in the root" 'ls "$M" | grep -qx status'
check "read-only" '! (echo x > "$M/status") 2>/dev/null'
check "reports the snapshots" 'grep -q "^snapshots: " "$M/status"'

echo "garbage collection"
chunks_in_store() { find "$B/store" -type f ! -name kv-chunk-fs.json | wc -l; }
gc_now() {  # SIGUSR2, then wait for the next "gc:" log line
  local n; n="$(grep -c 'event=gc' "$T/mount.log")"
  kill -USR2 "$MPID"
  for _ in $(seq 100); do [ "$(grep -c 'event=gc' "$T/mount.log")" -gt "$n" ] && return 0; sleep 0.1; done
  return 1
}
head -c $((8 << 20)) /dev/urandom > "$T/g.src"
cp "$T/g.src" "$M/model/g.bin"; before="$(chunks_in_store)"
rm "$M/model/g.bin"; gc_now
after="$(chunks_in_store)"
check "a deleted snapshot's chunks are collected ($before -> $after chunks)" '[ "$after" -lt "$((before - 50))" ]'
check "other snapshots are untouched" 'cmp -s "$T/b.src" "$M/model/a.bin" && cmp -s "$T/b.src" "$M/model/b.bin"'
cp "$T/g.src" "$M/model/g2.bin"
exec 8< "$M/model/g2.bin"
rm "$M/model/g2.bin"; gc_now
check "an open file's chunks survive gc after its unlink" '[ "$(md5sum <&8 | cut -d" " -f1)" = "$(md5sum < "$T/g.src" | cut -d" " -f1)" ]'
exec 8<&-
gc_now
check "... and are collected once it is closed" '[ "$(chunks_in_store)" -le "$after" ]'
check "status still healthy" '[ "$(head -1 "$M/status")" = healthy ]'

echo "recovery at start"
umount_fs
victim="$(grep -l . "$B"/tree/model/b.bin >/dev/null && find "$B/store" -type f ! -name kv-chunk-fs.json | head -1)"
rm -f "$victim"
mkdir -p "$B/store/0000"; : > "$B/store/0000/.tmp-cacnk-crash"
: > "$B/staging/data-crash"
mount_fs
check "a snapshot with a missing chunk is removed" 'grep -q "removing broken snapshot" "$T/mount.log"'
check "every remaining snapshot reads completely" 'for f in "$M"/model/*.bin; do cat "$f" > /dev/null || exit 1; done'
check "interrupted chunk writes are cleaned up" '[ ! -e "$B/store/0000/.tmp-cacnk-crash" ]'
check "leftover staging files are cleaned up" '[ ! -e "$B/staging/data-crash" ]'
check "status healthy after recovery" '[ "$(head -1 "$M/status")" = healthy ] && grep -q "broken snapshots removed" "$M/status"'
umount_fs

echo "store marker"
mkdir -p "$T/other/store" "$T/other-mnt"
printf '{"format":1,"digest":"sha512-256"}\n' > "$T/other/store/kv-chunk-fs.json"
check "a store made with another chunk hash is refused" '! timeout 10 "$BIN" mount --backing "$T/other" "$T/other-mnt" > "$T/other.log" 2>&1 && grep -q "digest" "$T/other.log"'

echo "configuration"
printf 'CACHE_MIB=11\nGC_INTERVAL=5m\nAPI_HOST=10.0.0.1\n' > "$T/kv.env"
cfg="$(KVCFS_GC_INTERVAL=7m KVCFS_API_HOST=127.0.0.2 "$BIN" config --config "$T/kv.env" --api-host 127.0.0.3)"
check "file over default" 'grep -q "^CACHE_MIB=11.*# file" <<< "$cfg"'
check "environment over file" 'grep -q "^GC_INTERVAL=7m.*# env KVCFS_GC_INTERVAL" <<< "$cfg"'
check "flag over environment" 'grep -q "^API_HOST=127.0.0.3.*# flag --api-host" <<< "$cfg"'
printf 'cache_mib: 12\napi:\n  port: 9999\n' > "$T/kv.yaml"
check "YAML file, nested keys" '"$BIN" config --config "$T/kv.yaml" | grep -q "^API_PORT=9999"'
check "an unknown key is refused" '! printf "NOPE=1\n" > "$T/bad.env" || ! "$BIN" config --config "$T/bad.env" >/dev/null 2>&1'

echo "api"
export KVCFS_API_SOCKET="$T/api.sock" KVCFS_API_HOST=127.0.0.1
KVCFS_API_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
export KVCFS_API_PORT
mount_fs
api() { curl -s --unix-socket "$T/api.sock" "$@"; }
check "status over the Unix socket" '[ "$(api http://x/status | python3 -c "import json,sys; print(json.load(sys.stdin)[\"health\"])")" = healthy ]'
check "status over TCP" 'curl -s "http://127.0.0.1:$KVCFS_API_PORT/status" | grep -q "\"health\": \"healthy\""'
check "status file lists the listeners" 'grep -q "^api:.*unix:$T/api.sock" "$M/status"'
# synthetic llama.cpp slot files: same system prompt, different conversations
mkdir -p "$M/llm"
python3 - "$M/llm" <<'PY'
import os, random, struct, sys
d = sys.argv[1]
rnd = random.Random(7)
system = [rnd.randrange(150000) for _ in range(40000)]   # 160 KB of tokens
def slot(name, toks):
    env = [-1, 1, len(toks)] + toks + [0]
    body = struct.pack("<III", 0x67677371, 4, len(env)) + struct.pack("<%di" % len(env), *env)
    with open(os.path.join(d, name), "wb") as f:
        f.write(body + os.urandom(2 << 20))
slot("s1.bin", system + [1, 2, 3])
slot("s2.bin", system + [9, 9])
slot("s3.bin", system + [1, 2, 3, 4, 5])
open(os.path.join(d, "junk.bin"), "wb").write(os.urandom(300000))
PY
snap() { api "http://x/snapshots/llm/$1" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d$2)"; }
check "snapshots listed with statistics" '[ "$(api "http://x/snapshots?model=llm" | python3 -c "import json,sys; print(json.load(sys.stdin)[\"total\"])")" = 4 ]'
check "shared system prompt shows as shared bytes" '[ "$(snap s1.bin "[\"shared_bytes\"]")" -gt 0 ] && [ "$(snap s1.bin "[\"unique_bytes\"]")" -gt 2000000 ]'
check "token count parsed from the header" '[ "$(snap s3.bin "[\"tokens\"]")" = 40005 ]'
check "a file that is not a slot file says why" 'snap junk.bin "[\"format\"]" | grep -q "unparsed: not a llama.cpp slot file"'
tree="$(api "http://x/tree?model=llm&summary=1")"
check "tree: 3 snapshots share the 40,000-token system prompt" 'python3 -c "import json,sys; s=json.loads(sys.argv[1])[\"models\"][0][\"summary\"]; assert {\"tokens\":40000,\"snapshots\":3} in s, s" "$tree"'
check "tree: s1 and s3 share 40,003 tokens" 'python3 -c "import json,sys; s=json.loads(sys.argv[1])[\"models\"][0][\"summary\"]; assert {\"tokens\":40003,\"snapshots\":2} in s, s" "$tree"'
check "tree: the junk file is listed as unparsed" 'python3 -c "import json,sys; u=json.loads(sys.argv[1])[\"models\"][0][\"unparsed\"]; assert \"llm/junk.bin\" in u" "$tree"'
cat "$M/llm/s1.bin" > /dev/null
check "a read counts as a use" '[ "$(snap s1.bin "[\"uses\"]")" = 1 ] && [ -n "$(snap s1.bin ".get(\"last_use\",\"\")")" ]'
head -c 20000 "$M/llm/s3.bin" > /dev/null
check "a header-only read (llama-server peeks at the tokens) is not a use" '[ "$(snap s3.bin "[\"uses\"]")" = 0 ]'
# a restore like llama-server's: header check, state, checkpoints to the end, each
# open also reading the last bytes - one use
python3 -c "f=open('$M/llm/s3.bin','rb'); f.read(20000); f.seek(-16,2); f.read()"
python3 -c "f=open('$M/llm/s3.bin','rb'); f.read(1500000); f.seek(-16,2); f.read()"
python3 -c "f=open('$M/llm/s3.bin','rb'); f.seek(1500000); f.read()"
check "a restore in three opens (each touching the end) is one use" '[ "$(snap s3.bin "[\"uses\"]")" = 1 ]'
check "label set through the API" '[ "$(curl -s -o /dev/null -w "%{http_code}" --unix-socket "$T/api.sock" -X PUT --data "system-prompt" http://x/snapshots/llm/s1.bin/labels/kind)" = 204 ]'
check "  ... visible as an xattr" '[ "$(python3 -c "import os; print(os.getxattr(\"$M/llm/s1.bin\", \"user.kvchunkfs.label.kind\").decode())")" = system-prompt ]'
cp "$M/llm/s1.bin" "$M/llm/s1.bin.tmp" && mv "$M/llm/s1.bin.tmp" "$M/llm/s1.bin"
check "uses and labels survive the proxy's save (.tmp renamed over)" '[ "$(snap s1.bin "[\"labels\"][\"kind\"]")" = system-prompt ] && [ "$(snap s1.bin "[\"uses\"]")" -ge 1 ]'
check "an invalid label name is refused" '[ "$(curl -s -o /dev/null -w "%{http_code}" --unix-socket "$T/api.sock" -X PUT "http://x/snapshots/llm/s1.bin/labels/a%20b?value=x")" = 400 ]'
check "label removed through the API" '[ "$(curl -s -o /dev/null -w "%{http_code}" --unix-socket "$T/api.sock" -X DELETE http://x/snapshots/llm/s1.bin/labels/kind)" = 204 ] && [ "$(snap s1.bin ".get(\"labels\")")" = None ]'
check "sorting and limit" '[ "$(api "http://x/snapshots?model=llm&sort=size&order=asc&limit=1" | python3 -c "import json,sys; print(json.load(sys.stdin)[\"snapshots\"][0][\"id\"])")" = llm/junk.bin ]'
check "a score per snapshot" 'snap s2.bin "[\"score\"]" | grep -qE "^[0-9.]+$"'
check "delete through the API" '[ "$(curl -s -o /dev/null -w "%{http_code}" --unix-socket "$T/api.sock" -X DELETE http://x/snapshots/llm/s2.bin)" = 204 ] && [ ! -e "$M/llm/s2.bin" ]'
check "  ... a missing snapshot is 404" '[ "$(curl -s -o /dev/null -w "%{http_code}" --unix-socket "$T/api.sock" -X DELETE http://x/snapshots/llm/s2.bin)" = 404 ]'
logs="$(api "http://x/logs?type=delete")"
check "the delete is an event in the log buffer" 'python3 -c "import json,sys; d=json.loads(sys.argv[1]); e=d[\"entries\"]; assert e and e[-1][\"fields\"][\"file\"]==\"llm/s2.bin\", d" "$logs"'
next="$(python3 -c "import json,sys; print(json.loads(sys.argv[1])[\"next\"])" "$logs")"
check "the log cursor returns only newer entries" '[ "$(api "http://x/logs?since=$next" | python3 -c "import json,sys; print(len(json.load(sys.stdin)[\"entries\"]))")" = 0 ]'
check "write events with timings" 'api "http://x/logs?type=write&limit=1" | grep -q "\"seconds\""'
check "metrics" 'api http://x/metrics | grep -q "^kvcfs_writes_total{model=\"llm\"}" && api http://x/metrics | grep -q "^kvcfs_deletes_total{model=\"llm\"} 1" && api http://x/metrics | grep -q "^kvcfs_read_seconds_bucket"'
check "POST /gc" 'curl -s --unix-socket "$T/api.sock" -X POST http://x/gc | grep -q "\"removed_chunks\""'
umount_fs
check "the socket is removed on unmount" '[ ! -e "$T/api.sock" ]'
unset KVCFS_API_SOCKET KVCFS_API_HOST KVCFS_API_PORT

echo
echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
