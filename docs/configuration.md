# Configuration

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
