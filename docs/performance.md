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

## Long contexts: re-processing vs loading from disk

A 35B-A3B mixture-of-experts model with multi-token prediction, 262,144-token context,
q8_0 KV, on an RTX 3090 (21.5 of 24.6 GB used), live behind llama-swap and a KV-cache
proxy; the model was unloaded and loaded again between the turns:

| conversation | re-processing (cold) | loading from disk (kv-chunk-fs) | faster |
|---|---|---|---|
| 12K tokens | ~4.3 s (2,841 t/s) | 0.97 s, 0.59 GB | ~4× |
| 48.5K tokens | 20.5 s (2,368 t/s) | **1.41 s** (34,600 t/s), 0.84 GB | **15×** |
| 119K tokens | 62 s (1,902 t/s) | **2.63 s** (45,200 t/s), 1.88 GB | **24×** |
| 249K tokens | 184 s (1,354 t/s) | **4.89 s** (51,000 t/s), 3.79 GB | **38×** |

After the restore only the new tokens are processed (22 here). Re-processing gets slower
per token as the context grows; loading gets faster per token (it is bandwidth: ~0.6–0.8
GB/s through the mount). A new session extending the 249K conversation by 3,204 tokens
reused 248,569 of its 251,773 prompt tokens and processed the rest in 4.4 s.

| saves at that size | snapshot | new chunks written | save time |
|---|---|---|---|
| 48.5K, next turn | 1.03 GB | 0.20 GB | 2.0 s |
| 119K, next turn | 2.08 GB | 0.20 GB | 4.2 s |
| 249K, next turn | 3.98 GB | 0.20 GB | 8.2 s |
| 252K, the extending session | 4.16 GB | 0.23 GB | 9.5 s |

Deduplication keeps the disk writes at ~0.2 GB per turn at any size, but the save time
grows with the whole snapshot (~0.45 GB/s: every byte is chunked and hashed), and a
proxy that saves while holding its lock makes a follow-up wait that long — up to ~9 s at
250K. Three remedies need no kv-chunk-fs changes, all in the
[local-llm-kv-cache](https://github.com/the-code-foundry/local-llm-kv-cache) fork:
`kv-save-growth` (skip a save until the conversation has grown by N tokens),
`kv-save-interval` (skip a save until N seconds have passed since the last one; e.g.
`KV_SAVE_INTERVAL=300` with `KV_SAVE_GROWTH=2000` rewrites the multi-GB file at most
once per five minutes), and the `terminal` save policy (one save per answer instead of
one per tool-call round). Skipped saves stay hot in the slot and are persisted by the
proxy's eviction and shutdown flushes. Snapshots also grow ~0.2 GB per turn beyond the
tokens' own share:
llama.cpp appends a context checkpoint each turn (up to 32 per slot).

## Concurrent conversations in multiple slots

The same 35B-A3B model with q4_0 KV and two slots of 262,144 tokens each (22.9 GB on the
RTX 3090), behind llama-swap and the KV-cache proxy. Four conversations, each a ~8.7K-token
document with its own secret word, every turn asking for it — a slot holding the wrong
conversation's state would answer wrongly:

- Building all four one after another: each processed from scratch (8.7K tokens,
  3.2–3.7 s); the third and fourth took the slots of the first two, which were saved
  first.
- Resuming them one at a time and in concurrent pairs: every turn restored from its
  snapshot — 30 tokens processed, 1.2–2.8 s single, 2.0–6.1 s per concurrent pair (the
  proxy prepares requests one at a time; generation overlaps).
- After a model unload: two conversations restored from chunks at the same time into
  different slots, then the other two.

All 20 turns answered with the right secret word, every resumed turn processed only its
30 new tokens, and slots were assigned freely between turns without crossing states.
