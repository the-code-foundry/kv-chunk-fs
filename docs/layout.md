# Layout and how it works

## On-disk layout

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
curl -s 127.0.0.1:9474/snapshots?sort=score | jq    # the API (docs/api.md)
journalctl -u kv-chunk-fs -f      # events: write, read, delete, gc, broken, error
systemctl kill -s USR2 kv-chunk-fs   # garbage collection now (or POST /gc)
systemctl kill -s USR1 kv-chunk-fs   # cache statistics to the log
```

## The mount

The mount mirrors `tree/` through go-fuse's loopback filesystem: directories, plain
files, rename, unlink, permissions and timestamps behave like ext4. Files matching
`--chunk-glob` (default `*.bin,*.bin.tmp`) are **stubs**: the file in the tree holds the
chunk index (casync `.caibx`), the xattr `user.kvchunkfs.size` the logical size, so
rename (a proxy's `.tmp` → final), unlink and LRU `utimens` stay atomic plain operations.
Small non-snapshot files (manifests and the like) are stored plainly.

## Writes

Writes stream: bytes arriving in order are queued (32 MiB per writer) and chunked by
[desync](https://github.com/folbricht/desync)'s content-defined chunker as they come;
only chunks the store lacks are written; the new index replaces the stub atomically when
the file is closed. llama-server saves in two passes (state, then an `O_APPEND` reopen
for the context checkpoints); the append continues the existing chunk list. Anything
else (out-of-order writes, partial truncates) falls back to a staging copy, chunked on
close.

## Reads

Reads prefetch the next 32 MiB of chunks with 8 workers and keep decoded chunks in a
256 MiB cache, so each chunk is read and decoded once per restore regardless of FUSE
request size. `VERIFY=true` checks each chunk's hash when it is read from disk.

## Garbage collection

Garbage collection (hourly, `SIGUSR2`, at start) deletes chunks no stub and no open file
needs; at start it also removes interrupted writes and stubs whose chunks are missing (a
cache miss for the proxy). It never runs while a writer is adding chunks.
