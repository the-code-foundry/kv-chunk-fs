package kvfs

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// StatusName is the virtual file in the mount's root. Its first line is
// "healthy" or "degraded: <reason>"; the rest is key: value lines. It is
// generated when opened, so `head -1 <mount>/status` is a health check.
const StatusName = "status"

// StatusData is the mount's health and statistics (the status file, and the
// API's GET /status).
type StatusData struct {
	Health     string    `json:"health"` // "healthy" or "degraded"
	Problems   []string  `json:"problems,omitempty"`
	Version    string    `json:"version"`
	Started    time.Time `json:"started"`
	Uptime     string    `json:"uptime"`
	Mount      string    `json:"mount"`
	Backing    string    `json:"backing"`
	Chunked    []string  `json:"chunked"`
	API        []string  `json:"api,omitempty"`
	LastGC     *GCView   `json:"last_gc,omitempty"`
	Pinned     int       `json:"open_pinned_files"`
	CacheHits  int64     `json:"cache_hits"`
	CacheMiss  int64     `json:"cache_misses"`
	ChunksRead int64     `json:"chunks_read"`
	BytesRead  int64     `json:"chunk_bytes_read"`
	Errors     int       `json:"errors"`
	LastError  string    `json:"last_error,omitempty"`
}

// GCView is a garbage collection's result for display.
type GCView struct {
	At           time.Time `json:"at"`
	Seconds      float64   `json:"seconds"`
	Snapshots    int       `json:"snapshots"`
	LogicalBytes int64     `json:"logical_bytes"`
	Chunks       int       `json:"chunks"`
	StoreBytes   int64     `json:"store_bytes"`
	Dedup        float64   `json:"dedup_ratio"`
	Removed      int       `json:"removed_chunks"`
	RemovedBytes int64     `json:"removed_bytes"`
	Broken       int       `json:"broken_removed"`
	Error        string    `json:"error,omitempty"`
}

// Status collects the mount's health and statistics.
func (k *FS) Status() StatusData {
	k.mu.Lock()
	gc, errs, lastErr, pinned := k.lastGC, k.errors, k.lastErr, len(k.pinned)
	apiAddrs := append([]string(nil), k.opt.APIAddrs...)
	k.mu.Unlock()
	st := k.store.Stats()

	var problems []string
	if f, err := os.CreateTemp(k.staging, "probe-*"); err != nil {
		problems = append(problems, "staging not writable: "+err.Error())
	} else {
		f.Close()
		os.Remove(f.Name())
	}
	if _, err := os.Stat(k.store.Dir()); err != nil {
		problems = append(problems, "store unreadable: "+err.Error())
	}
	if gc.Err != nil && gc.Err != errBusy {
		problems = append(problems, "last gc failed: "+gc.Err.Error())
	}
	if errs > 0 {
		problems = append(problems, fmt.Sprintf("%d errors since start", errs))
	}
	d := StatusData{
		Health: "healthy", Problems: problems, Version: k.opt.Version, Started: k.started,
		Uptime: time.Since(k.started).Round(time.Second).String(), Mount: k.opt.Mountpoint,
		Backing: k.opt.Backing, Chunked: k.opt.ChunkGlobs, API: apiAddrs, Pinned: pinned,
		CacheHits: st.Hits, CacheMiss: st.Misses, ChunksRead: st.Fetched, BytesRead: st.FetchedBytes,
		Errors: errs, LastError: lastErr,
	}
	if len(problems) > 0 {
		d.Health = "degraded"
	}
	if !gc.At.IsZero() {
		v := &GCView{At: gc.At, Seconds: round3(gc.Took.Seconds()), Snapshots: gc.Stubs,
			LogicalBytes: gc.LogicalBytes, Chunks: gc.Chunks, StoreBytes: gc.StoreBytes,
			Removed: gc.Removed, RemovedBytes: gc.RemovedBytes, Broken: gc.Broken}
		if gc.StoreBytes > 0 {
			v.Dedup = float64(int64(float64(gc.LogicalBytes)/float64(gc.StoreBytes)*100)) / 100
		}
		if gc.Err != nil {
			v.Error = gc.Err.Error()
		}
		d.LastGC = v
	}
	return d
}

// status renders the status file: first line healthy or degraded: <reasons>.
func (k *FS) status() []byte {
	d := k.Status()
	var b strings.Builder
	if d.Health == "healthy" {
		b.WriteString("healthy\n")
	} else {
		b.WriteString("degraded: " + strings.Join(d.Problems, "; ") + "\n")
	}
	kv := func(key, format string, args ...any) {
		fmt.Fprintf(&b, "%-14s %s\n", key+":", fmt.Sprintf(format, args...))
	}
	kv("version", "%s", d.Version)
	kv("uptime", "%s", d.Uptime)
	kv("mount", "%s", d.Mount)
	kv("backing", "%s", d.Backing)
	kv("chunked", "%s", strings.Join(d.Chunked, ","))
	if len(d.API) > 0 {
		kv("api", "%s", strings.Join(d.API, ", "))
	} else {
		kv("api", "off")
	}
	if g := d.LastGC; g != nil {
		kv("snapshots", "%d, %s logical", g.Snapshots, gib(g.LogicalBytes))
		kv("store", "%d chunks, %s on disk (dedup %.2fx)", g.Chunks, gib(g.StoreBytes), g.Dedup)
		res := fmt.Sprintf("removed %d chunks (%s), %d broken snapshots removed", g.Removed, gib(g.RemovedBytes), g.Broken)
		if g.Error != "" {
			res = g.Error
		}
		kv("last gc", "%s, %.3fs, %s", g.At.Format(time.RFC3339), g.Seconds, res)
	}
	kv("open files", "%d pinned (open readers, appends)", d.Pinned)
	kv("cache", "%d hits, %d misses, %d chunks read from disk (%s)", d.CacheHits, d.CacheMiss, d.ChunksRead, gib(d.BytesRead))
	kv("errors", "%d since start", d.Errors)
	if d.LastError != "" {
		kv("last error", "%s", d.LastError)
	}
	return []byte(b.String())
}

// statusNode is the virtual status file.
type statusNode struct {
	fs.Inode
	kfs *FS
}

var (
	_ = (fs.NodeGetattrer)((*statusNode)(nil))
	_ = (fs.NodeOpener)((*statusNode)(nil))
)

func (s *statusNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFREG | 0o444
	out.Size = uint64(len(s.kfs.status()))
	now := time.Now()
	out.SetTimes(&now, &now, &now)
	out.Owner = fuse.Owner{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
	return 0
}

// Open renders the file once per open; direct I/O so readers always get it fresh.
func (s *statusNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, syscall.EACCES
	}
	return &statusHandle{data: s.kfs.status()}, fuse.FOPEN_DIRECT_IO, 0
}

type statusHandle struct{ data []byte }

var _ = (fs.FileReader)((*statusHandle)(nil))

func (h *statusHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off >= int64(len(h.data)) {
		return fuse.ReadResultData(nil), 0
	}
	return fuse.ReadResultData(h.data[off:min(off+int64(len(dest)), int64(len(h.data)))]), 0
}

// SetAPIAddrs records the API's listeners for the status.
func (k *FS) SetAPIAddrs(addrs []string) {
	k.mu.Lock()
	k.opt.APIAddrs = addrs
	k.mu.Unlock()
}
