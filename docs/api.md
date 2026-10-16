# HTTP API

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

## Eviction

Eviction is left to other tools: `GET /snapshots?sort=score` lists the best candidates
first, `DELETE` removes one. The score is a suggestion:
`(1 + Wunique·unique_GiB + Wsize·size_GiB) · 2^(idle/half_life) / (1 + Wuses·uses)`,
where idle is the time since the last use (or since the snapshot was written). The
weights and half-life are settings (`SCORE_*`, see docs/configuration.md).
