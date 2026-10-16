package kvfs

import (
	"time"

	"kvchunkfs/internal/chunks"
	"kvchunkfs/internal/metrics"
)

type statsView = chunks.Stats

// fsMetrics are the Prometheus series of a mount.
type fsMetrics struct {
	reg            metrics.Registry
	writes         *metrics.CounterVec
	writeBytes     *metrics.CounterVec
	writeNewBytes  *metrics.CounterVec
	writeSeconds   *metrics.HistogramVec
	reads          *metrics.CounterVec
	readBytes      *metrics.CounterVec
	readSeconds    *metrics.HistogramVec
	deletes        *metrics.CounterVec
	gcRuns         *metrics.CounterVec
	gcRemoved      *metrics.CounterVec
	gcRemovedBytes *metrics.CounterVec
	gcBroken       *metrics.CounterVec
	errors         *metrics.CounterVec
}

var secondsBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30}

func newMetrics(k *FS) *fsMetrics {
	m := &fsMetrics{}
	r := &m.reg
	r.Info("kvcfs_build_info", "kv-chunk-fs version.", map[string]string{"version": k.opt.Version})
	m.writes = r.Counter("kvcfs_writes_total", "Chunked files written (llama-server saves a snapshot in two writes: state, then appended checkpoints).", "model")
	m.writeBytes = r.Counter("kvcfs_write_bytes_total", "Logical bytes of chunked files written.", "model")
	m.writeNewBytes = r.Counter("kvcfs_write_new_bytes_total", "Bytes of new chunks stored by writes (the rest was deduplicated).", "model")
	m.writeSeconds = r.Histogram("kvcfs_write_seconds", "Time from open to the index being written.", secondsBuckets, "model")
	m.reads = r.Counter("kvcfs_reads_total", "Chunked files read (restores).", "model")
	m.readBytes = r.Counter("kvcfs_read_bytes_total", "Bytes read from chunked files.", "model")
	m.readSeconds = r.Histogram("kvcfs_read_seconds", "Time from open to close of a read.", secondsBuckets, "model")
	m.deletes = r.Counter("kvcfs_deletes_total", "Snapshots deleted.", "model")
	m.gcRuns = r.Counter("kvcfs_gc_runs_total", "Garbage collections, by result.", "result")
	m.gcRemoved = r.Counter("kvcfs_gc_removed_chunks_total", "Chunks removed by garbage collection.")
	m.gcRemovedBytes = r.Counter("kvcfs_gc_removed_bytes_total", "Bytes removed by garbage collection.")
	m.gcBroken = r.Counter("kvcfs_gc_broken_snapshots_total", "Snapshots removed because chunks were missing.")
	m.errors = r.Counter("kvcfs_errors_total", "Errors.")

	gc := func(f func(GCResult) float64) func() float64 {
		return func() float64 {
			k.mu.Lock()
			defer k.mu.Unlock()
			return f(k.lastGC)
		}
	}
	r.Gauge("kvcfs_store_bytes", "Chunk bytes on disk, as of the last garbage collection.", gc(func(g GCResult) float64 { return float64(g.StoreBytes) }))
	r.Gauge("kvcfs_store_chunks", "Chunks in the store, as of the last garbage collection.", gc(func(g GCResult) float64 { return float64(g.Chunks) }))
	r.Gauge("kvcfs_logical_bytes", "Logical size of all snapshots, as of the last garbage collection.", gc(func(g GCResult) float64 { return float64(g.LogicalBytes) }))
	r.Gauge("kvcfs_snapshots", "Snapshots, as of the last garbage collection.", gc(func(g GCResult) float64 { return float64(g.Stubs) }))
	r.Gauge("kvcfs_gc_last_seconds", "Duration of the last garbage collection.", gc(func(g GCResult) float64 { return g.Took.Seconds() }))
	r.Gauge("kvcfs_gc_last_timestamp_seconds", "When the last garbage collection ran.", gc(func(g GCResult) float64 {
		if g.At.IsZero() {
			return 0
		}
		return float64(g.At.Unix())
	}))
	st := func(f func(s statsView) float64) func() float64 {
		return func() float64 { return f(k.store.Stats()) }
	}
	r.Gauge("kvcfs_cache_hits", "Chunk reads served from the decoded-chunk cache.", st(func(s statsView) float64 { return float64(s.Hits) }))
	r.Gauge("kvcfs_cache_misses", "Chunk reads that went to disk.", st(func(s statsView) float64 { return float64(s.Misses) }))
	r.Gauge("kvcfs_chunk_read_bytes", "Chunk bytes read from disk.", st(func(s statsView) float64 { return float64(s.FetchedBytes) }))
	r.Gauge("kvcfs_open_pinned_files", "Open files pinning chunks.", func() float64 {
		k.mu.Lock()
		defer k.mu.Unlock()
		return float64(len(k.pinned))
	})
	r.Gauge("kvcfs_uptime_seconds", "Seconds since start.", func() float64 { return time.Since(k.started).Seconds() })
	return m
}
