package kvfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/folbricht/desync"
)

// GCResult describes one garbage collection.
type GCResult struct {
	At           time.Time
	Took         time.Duration
	Stubs        int   // chunked files in the tree
	Broken       int   // stubs removed because chunks they need were missing
	Chunks       int   // chunks in the store after the sweep
	Removed      int   // unreferenced chunks deleted
	RemovedBytes int64 //
	StoreBytes   int64 // chunk bytes on disk after the sweep
	LogicalBytes int64 // sum of the chunked files' sizes
	Err          error
}

// errBusy: a writer kept the store busy for the whole wait; try again later.
var errBusy = errors.New("writers busy, skipped")

// GC deletes chunks no stub and no open file refers to, and removes stubs whose
// chunks are missing (after a crash, or damage): to the kv-cache proxy a
// missing snapshot is a cache miss, while a broken one fails every restore. It
// runs only while no writer is adding chunks; if writers stay busy for wait,
// it skips this round rather than hold up saves.
func (k *FS) GC(wait time.Duration) GCResult {
	res := GCResult{At: time.Now()}
	deadline := time.Now().Add(wait)
	for !k.store.TryExclusive(func() error { return k.collect(&res) }) {
		if time.Now().After(deadline) {
			res.Err = errBusy
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	res.Took = time.Since(res.At)
	k.mu.Lock()
	k.lastGC = res
	k.mu.Unlock()
	if res.Err != nil {
		k.m.gcRuns.Inc("skipped")
		k.ev.Emit("info", "gc", "gc: "+res.Err.Error(), nil)
	} else {
		k.m.gcRuns.Inc("ok")
		k.m.gcRemoved.Add(float64(res.Removed))
		k.m.gcRemovedBytes.Add(float64(res.RemovedBytes))
		k.ev.Emit("info", "gc", "gc", map[string]any{
			"snapshots": res.Stubs, "logical_bytes": res.LogicalBytes, "chunks": res.Chunks,
			"store_bytes": res.StoreBytes, "removed_chunks": res.Removed, "removed_bytes": res.RemovedBytes,
			"broken": res.Broken, "seconds": round3(res.Took.Seconds())})
	}
	return res
}

func (k *FS) collect(res *GCResult) error {
	list, err := k.store.List()
	if err != nil {
		res.Err = err
		return err
	}
	present := make(map[desync.ChunkID]struct{}, len(list))
	for _, c := range list {
		present[c.ID] = struct{}{}
	}
	refs := map[desync.ChunkID]struct{}{}
	for _, cs := range k.pinnedChunks() {
		for _, c := range cs {
			refs[c.ID] = struct{}{}
		}
	}
	err = filepath.WalkDir(k.tree, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		size, ok := stubSize(path)
		if !ok {
			return nil
		}
		var cs []desync.IndexChunk
		if size > 0 {
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			idx, err := desync.IndexFromReader(f)
			f.Close()
			if err != nil {
				return k.dropBroken(res, path, fmt.Sprintf("unreadable index: %v", err))
			}
			cs = idx.Chunks
		}
		for _, c := range cs {
			if _, ok := present[c.ID]; !ok {
				return k.dropBroken(res, path, fmt.Sprintf("chunk %s missing", c.ID))
			}
		}
		for _, c := range cs {
			refs[c.ID] = struct{}{}
		}
		res.Stubs++
		res.LogicalBytes += size
		return nil
	})
	if err != nil {
		res.Err = err
		return err
	}
	for _, c := range list {
		if _, ok := refs[c.ID]; ok {
			res.Chunks++
			res.StoreBytes += c.Size
			continue
		}
		if err := k.store.Remove(c); err != nil {
			k.fail("gc: remove %s: %v", c.Path, err)
			continue
		}
		res.Removed++
		res.RemovedBytes += c.Size
	}
	return nil
}

// dropBroken is recovery, not an error: the proxy sees a cache miss and saves
// again. It is counted in GCResult.Broken (and the status file), not as an error.
func (k *FS) dropBroken(res *GCResult, path, why string) error {
	rel := k.rel(path)
	k.ev.Emit("info", "broken", "removing broken snapshot", map[string]any{"model": modelOf(rel), "file": rel, "reason": why})
	k.m.gcBroken.Inc()
	if err := os.Remove(path); err == nil {
		res.Broken++
	}
	return nil
}

// pin keeps chunks referenced while an open file needs them, even if its stub
// is deleted or replaced meanwhile.
func (k *FS) pin(key any, cs []desync.IndexChunk) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pinned == nil {
		k.pinned = map[any][]desync.IndexChunk{}
	}
	k.pinned[key] = cs
}

func (k *FS) unpin(key any) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.pinned, key)
}

func (k *FS) pinnedChunks() [][]desync.IndexChunk {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([][]desync.IndexChunk, 0, len(k.pinned))
	for _, cs := range k.pinned {
		out = append(out, cs)
	}
	return out
}

// fail logs an error and counts it for the status file.
func (k *FS) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	k.ev.Emit("error", "error", msg, nil)
	k.m.errors.Inc()
	k.mu.Lock()
	k.errors++
	k.lastErr = time.Now().Format(time.RFC3339) + " " + msg
	k.mu.Unlock()
}

func gib(b int64) string { return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30)) }

