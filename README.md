# kv-chunk-fs

A deduplicating FUSE filesystem for llama.cpp KV-cache snapshots. To llama-server and a
KV-cache proxy such as
[local-llm-kv-cache](https://github.com/the-code-foundry/local-llm-kv-cache) it is an
ordinary directory; underneath, every slot snapshot is stored as content-defined chunks,
written while llama-server saves, so a conversation turn writes only what changed and
snapshots that share a system prompt share its chunks.

Article: [kv-chunk-fs — deduplicating llama.cpp KV-cache snapshots](https://vlad.babii.ro/en/showcase/kv-chunk-fs/)

## Why

llama.cpp slot snapshots are full slot states, rewritten completely on every save: one
file per conversation, overwritten, growing with the conversation (~300 MB–1.2 GB).
Most of those bytes do not change between turns — measured, a turn writes ~36% new data
on a 9B hybrid model with recurrent state and ~6% on Qwen3-1.7B, and two conversations
sharing a system prompt share ~36% of their chunks. kv-chunk-fs stores and writes those
bytes once, keeps restores fast with read-ahead and a decoded-chunk cache, and leaves
the files looking ordinary: rename, unlink, `utimens`, `find` and `rm` all behave.

## Quick start

```sh
./build.sh            # vet, unit tests, static build in the golang Docker container
sudo deploy/install.sh
head -1 /srv/kv/cache/status      # healthy
```

Point `--slot-save-path` (and the proxy's cache dir) at a model directory inside the
mount; nothing else changes. Details in [docs/install.md](docs/install.md).

## Performance

541 MB snapshot (12,964 tokens, q4_0 KV), direct llama-server slot save/restore, NVMe:

| | plain ext4 | kv-chunk-fs |
|---|---|---|
| save | 0.51 s | 1.16 s |
| restore, cold | 0.37 s | 0.62 s |
| restore, warm | 0.26 s | 0.26 s |

Numbers, dedup measurements and what they cost in practice: [docs/performance.md](docs/performance.md).

## Documentation

| | |
|---|---|
| [docs/layout.md](docs/layout.md) | on-disk layout, stubs, the write/read paths, garbage collection |
| [docs/install.md](docs/install.md) | build, systemd service, mount propagation, container example |
| [docs/configuration.md](docs/configuration.md) | every setting, config file / environment / flags |
| [docs/api.md](docs/api.md) | HTTP API, Prometheus metrics, eviction scores |
| [docs/metadata.md](docs/metadata.md) | per-snapshot xattr metadata, events, token-prefix trees |
| [docs/performance.md](docs/performance.md) | dedup and timing measurements |
| [docs/development.md](docs/development.md) | building, tests, code layout |

Related: [the-code-foundry/local-llm-kv-cache](https://github.com/the-code-foundry/local-llm-kv-cache),
the disk-backed KV-cache proxy this filesystem was built under.
