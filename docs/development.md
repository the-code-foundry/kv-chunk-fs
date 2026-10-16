# Development

```sh
./build.sh                    # vet, unit tests, build (./build.sh test: no build)
TEST_DIR=/tmp ./test-fs.sh    # 75 FUSE and API checks on a scratch mount; needs /dev/fuse, curl
bin/kv-chunk-fs mount --backing DIR MNT [--debug]   # by hand; mount -h lists every setting
bin/kv-chunk-fs config --config FILE                # effective settings and their sources
```

`build.sh` runs everything in the `golang` Docker container, so no Go toolchain is
needed on the host. On rootless Docker, run the `go` commands without the `-u` flag the
script passes (rootless maps the container root to your user already).

## Code layout

| package | |
|---|---|
| `cmd/kv-chunk-fs/` | CLI: `mount`, `config` |
| `internal/chunks` | chunk store, decoded-chunk cache, read-ahead, streaming writes, gc primitives |
| `internal/kvfs` | FUSE nodes, garbage collection, status file, metadata, statistics |
| `internal/api` | HTTP API |
| `internal/config` | settings: defaults, config file, environment, flags |
| `internal/events` | journal + ring buffer |
| `internal/metrics` | Prometheus text |
| `internal/slotfile` | llama.cpp snapshot header, token-prefix trees |
