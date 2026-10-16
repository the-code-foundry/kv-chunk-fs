# Performance

## Dedup

Measured on llama.cpp slot snapshots (state format 4, q4_0/q8_0 KV):

- A conversation turn on a 9B hybrid model with recurrent state rewrites a 435–541 MB
  snapshot, of which ~36% is new chunks; on Qwen3-1.7B, ~6%. The new data is dominated
  by the recurrent state and new context checkpoints, not by the new tokens' KV.
- Two conversations sharing a 7.1K-token system prompt and tools share ~36% of their
  chunks. Recomputation of the same prefix is byte-identical, so cross-conversation
  dedup is reliable.
- zstd on these snapshots saves only 4–5%, so chunks are stored uncompressed.

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
