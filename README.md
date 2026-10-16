# kv-chunk-fs

A deduplicating FUSE filesystem for llama.cpp KV-cache snapshots. To llama-server and a
KV-cache proxy such as
[local-llm-kv-cache](https://github.com/the-code-foundry/local-llm-kv-cache) it is an
ordinary directory; underneath, every slot snapshot is stored as content-defined chunks,
written while llama-server saves, so a conversation turn writes only what changed
(measured: ~36% of a 435–541 MB snapshot for a 9B hybrid model with recurrent state,
~6% for Qwen3-1.7B) and snapshots that share a system prompt share its chunks.

llama.cpp slot snapshots are full slot states, rewritten completely on every save: one
file per conversation, overwritten, growing with the conversation (~300 MB–1.2 GB). Most
of those bytes do not change between turns. kv-chunk-fs stores them once.

## Layout

```
/srv/kv/cache          the mount (example; put it inside a plain parent directory)
  <model>/             llama-server --slot-save-path, the proxy's cache dir
  status               virtual file: health and statistics
/srv/kv-cache.backing
  tree/                what the mount shows; chunked files are stubs (index + size xattr)
  store/               content-defined chunks (uncompressed, SHA-256 IDs), kv-chunk-fs.json
  staging/             fallback copies for non-sequential writes
```

```sh
head -1 /srv/kv/cache/status      # healthy | degraded: <reasons>
cat /srv/kv/cache/status          # snapshots, store size, dedup ratio, last gc, cache, errors, api
curl -s 127.0.0.1:9474/snapshots?sort=score | jq    # the API (see below)
journalctl -u kv-chunk-fs -f      # events: write, read, delete, gc, broken, error
systemctl kill -s USR2 kv-chunk-fs   # garbage collection now (or POST /gc)
systemctl kill -s USR1 kv-chunk-fs   # cache statistics to the log
```

## How it works

- The mount mirrors `tree/` through go-fuse's loopback filesystem: directories, plain
  files, rename, unlink, permissions and timestamps behave like ext4. Files matching
  `--chunk-glob` (default `*.bin,*.bin.tmp`) are **stubs**: the file in the tree holds the
  chunk index (casync `.caibx`), the xattr `user.kvchunkfs.size` the logical size, so
  rename (a proxy's `.tmp` → final), unlink and LRU `utimens` stay atomic plain
  operations.
- **Writes** stream: bytes arriving in order are queued (32 MiB per writer) and chunked
  by [desync](https://github.com/folbricht/desync)'s content-defined chunker as they
  come; only chunks the store lacks are written; the new index replaces the stub
  atomically when the file is closed. llama-server saves in two passes (state, then an
  `O_APPEND` reopen for the context checkpoints); the append continues the existing chunk
  list. Anything else (out-of-order writes, partial truncates) falls back to a staging
  copy.
- **Reads** prefetch the next 32 MiB of chunks with 8 workers and keep decoded chunks in
  a 256 MiB cache.
- **Garbage collection** (hourly, `SIGUSR2`, at start) deletes chunks no stub and no open
  file needs; at start it also removes interrupted writes and stubs whose chunks are
  missing (a cache miss for the proxy). It never runs while a writer is adding chunks.

## Install

```sh
./build.sh            # vet, unit tests, static build in the golang Docker container
sudo deploy/install.sh
```

`build.sh` needs Docker, not a Go toolchain. `install.sh` installs the binary to
`/opt/kv-chunk-fs/`, enables `user_allow_other` in `/etc/fuse.conf`, installs
`/etc/kv-chunk-fs.env` (settings; never overwrites an existing copy), the systemd unit
and a drop-in ordering llama-swap after it, then restarts the service and waits for
`healthy`. Layout and service user are set at the top of `install.sh` and in
`/etc/kv-chunk-fs.env`.

Mount inside a plain parent directory (`/srv/kv/cache` inside `/srv/kv`) and have
containers bind the parent with propagation `rslave`: a bind of the mount itself would
keep the dead mount after a service restart, while a mount made under a bound directory
propagates into it. Restarting the service while models run is then fine. A container
alternative is in `deploy/docker/` (example only).

## Configuration

Every setting comes from, in increasing precedence: the built-in default, a config file
(`--config FILE` or `KVCFS_CONFIG`), the environment (`KVCFS_<NAME>`), a flag
(`--<name>`, lower case with dashes). Config files are `KEY=VALUE` lines (bash style,
`export` and quotes allowed; usable as a systemd `EnvironmentFile`) or YAML/JSON (nested
keys flatten: `api: {port: 9474}` is `API_PORT`). Keys may be written `CACHE_MIB`,
`cache_mib`, `cache-mib` or `KVCFS_CACHE_MIB`; an unknown key is an error.
`kv-chunk-fs config [--config FILE]` prints the effective values and where each came
from.

| setting | default | |
|---|---|---|
| `BACKING` | — | backing directory: `tree/`, `store/`, `staging/` (required) |
| `MOUNT` | — | mount point (or the positional argument) |
| `CHUNK_GLOB` | `*.bin,*.bin.tmp` | base-name patterns stored as chunks |
| `CHUNK_SIZE` | `16:64:256` | min:avg:max chunk size, KiB |
| `VERIFY` | `false` | check each chunk's hash when read from disk |
| `CACHE_MIB` | `256` | decoded-chunk cache |
| `READAHEAD_MIB` | `32` | prefetched past a sequential reader |
| `WORKERS` | `8` | goroutines prefetching and storing chunks |
| `ALLOW_OTHER` | `true` | let other users (dockerd) in; the kernel still checks permissions |
| `GC_INTERVAL` | `1h` | garbage collection interval (0: start, SIGUSR2, POST /gc only) |
| `GC_WAIT` | `1m` | how long a collection waits for busy writers before skipping |
| `API_HOST` / `API_PORT` | — | TCP listener when the port is set (host defaults to 127.0.0.1) |
| `API_SOCKET` | — | Unix socket listener when set |
| `LOG_BUFFER_LINES` | `1000` | log lines and events kept for `GET /logs` |
| `USE_WINDOW` | `30s` | reads of a snapshot within this window count as one use |
| `SCORE_UNIQUE_WEIGHT` | `1` | eviction score: weight of unique GiB |
| `SCORE_SIZE_WEIGHT` | `0` | eviction score: weight of logical GiB |
| `SCORE_USES_WEIGHT` | `1` | eviction score: weight of uses |
| `SCORE_HALF_LIFE` | `24h` | eviction score: idle time that doubles the score |
| `DEBUG` | `false` | log every FUSE request |

## API

Off unless `API_PORT` and/or `API_SOCKET` is set; no authentication (keep it local or
put something in front). JSON unless noted. A snapshot id is its path in the mount,
e.g. `qwen3-1.7b/local-llm-session-….bin`; the model is the first path component.

| endpoint | |
|---|---|
| `GET /status` | health (`healthy`/`degraded` + problems), version, uptime, listeners, last gc (snapshots, logical and store bytes, dedup ratio), cache, errors |
| `GET /metrics` | Prometheus text: `kvcfs_writes_total`, `kvcfs_write_seconds` (histogram), `kvcfs_write_bytes_total`, `kvcfs_write_new_bytes_total`, `kvcfs_reads_total`, `kvcfs_read_seconds`, `kvcfs_read_bytes_total`, `kvcfs_deletes_total` (all per `model`); `kvcfs_gc_*`, `kvcfs_store_bytes`, `kvcfs_logical_bytes`, `kvcfs_snapshots`, `kvcfs_cache_*`, `kvcfs_errors_total`, `kvcfs_uptime_seconds`, `kvcfs_build_info` |
| `GET /logs?since=&type=&level=&limit=` | the log buffer: `entries` (id, time, level, type, msg, fields) and `next`, the cursor for the next call; `type` is a comma-separated list (`write,read,delete,gc,broken,error,log`) |
| `GET /snapshots?model=&sort=&order=&limit=` | every snapshot: size, chunks, `unique_bytes` (freed by deleting it), `shared_bytes`, modified, uses, last_use, created, labels, idle_seconds, `score`, tokens, format; sort by `score` (default), `size`, `unique`, `uses`, `last_use`, `idle`, `tokens`, `id`; `order=asc` for ascending |
| `GET /snapshots/<id>` | one snapshot |
| `DELETE /snapshots/<id>` | delete it (through the mount: a `delete` event like any other) |
| `PUT /snapshots/<id>/labels/<label>` | set a label; value in the body or `?value=`; names `[A-Za-z0-9._-]{1,64}` |
| `DELETE /snapshots/<id>/labels/<label>` | remove a label |
| `GET /tree?model=&summary=1` | token-prefix tree per model and `summary` ("N snapshots share their first T tokens", largest groups first); `unparsed` lists files that are not llama.cpp slot files, with the reason |
| `POST /gc` | garbage collection now; 409 if writers stayed busy |

**Eviction** is left to other tools: `GET /snapshots?sort=score` lists the best
candidates first, `DELETE` removes one. The score is a suggestion:
`(1 + Wunique·unique_GiB + Wsize·size_GiB) · 2^(idle/half_life) / (1 + Wuses·uses)`,
where idle is the time since the last use (or since the snapshot was written).

## Snapshot metadata, events, trees

- **Metadata** is stored as extended attributes on the stub, readable with
  `getfattr -d -m user.kvchunkfs <mount>/<model>/<file>`: `uses`, `last_use`,
  `created` (Unix seconds), `label.<name>`, and `size`. It survives a rewrite and a
  rename over the file (a proxy's `.tmp` → final), so a conversation keeps its history
  across turns. A **use** is a read of at least 1 MiB (or half the file), counted at most
  once per `USE_WINDOW` (30 s): llama-server restores a snapshot in two or three opens
  within a second (a ~20 KB header check, the state, for some models the appended
  context checkpoints — each open also reading the end of the file), which is one
  restore; the header check alone is not a use.
- **Events** go to the journal (`event=write file=… mode=stream bytes=… new_bytes=…
  seconds=…`) and the log buffer: `write` (one per closed write; a model with context
  checkpoints saves in two: state, then the appended checkpoints), `read`, `delete`
  (with size, uses, age, idle), `gc`, `broken`, `error`.
- **Trees** come from each snapshot's header: llama.cpp state format 4 and llama-server's
  token envelope (`-1, 1, count, tokens…, media`). A file that does not parse is listed
  as `unparsed: <reason>` (`not a llama.cpp slot file`, `format version 5, expected 4`,
  `truncated header`, …); nothing fails because of it.

## Costs

A 9B hybrid model with recurrent state, 541 MB snapshot (12,964 tokens, q4_0 KV),
direct llama-server slot save/restore, NVMe:

| | plain ext4 | kv-chunk-fs |
|---|---|---|
| save | 0.51 s | 1.16 s |
| save over an existing file | 0.73 s | 1.17 s |
| restore, cold | 0.37 s | 0.62 s |
| restore, warm | 0.26 s | 0.26 s |

Qwen3-1.7B, ~800 MB snapshots (13,164 tokens, q8_0 KV), live behind llama-swap and a
KV-cache proxy: a write 1.6–2.0 s from open to the index being written, ~55 MB of it new
chunks; a restore read of 801 MB in 1.27 s.

The save is the cost that matters when a proxy saves after each answer while holding a
lock: a follow-up sent at once waits ~0.65 s longer than on plain ext4. The ceiling is
desync's serial chunker (~1 GB/s alone, ~560 MB/s with hashing and queueing).

## Development

```sh
./build.sh                    # vet, unit tests, build (./build.sh test: no build)
TEST_DIR=/tmp ./test-fs.sh    # 75 FUSE and API checks on a scratch mount; needs /dev/fuse, curl
bin/kv-chunk-fs mount --backing DIR MNT [--debug]   # by hand; mount -h lists every setting
bin/kv-chunk-fs config --config FILE                # effective settings and their sources
```

Code layout: `cmd/kv-chunk-fs/` (CLI), `internal/chunks` (store, cache, read-ahead,
streaming writes, gc primitives), `internal/kvfs` (FUSE nodes, gc, status, metadata,
statistics), `internal/api` (HTTP), `internal/config`, `internal/events` (journal + ring
buffer), `internal/metrics` (Prometheus text), `internal/slotfile` (llama.cpp snapshot
header, token-prefix trees).

Related: [the-code-foundry/local-llm-kv-cache](https://github.com/the-code-foundry/local-llm-kv-cache),
the disk-backed KV-cache proxy this filesystem was built under.
