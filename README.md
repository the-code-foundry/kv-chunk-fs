# kv-chunk-fs

A deduplicating FUSE filesystem for llama.cpp KV-cache snapshots. llama-server and the
[local-llm-kv-cache](https://github.com/the-code-foundry/local-llm-kv-cache) proxy keep
seeing ordinary snapshot files; underneath, the data is stored as content-defined chunks,
so the bytes a turn did not change — earlier tokens, shared system-prompt and tool
prefixes — are stored and written once.

Status: design phase.
