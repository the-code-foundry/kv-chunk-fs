// Package kvfs is the FUSE side of kv-chunk-fs.
//
// The mount mirrors <backing>/tree through go-fuse's loopback filesystem, so
// directories, plain files, rename, unlink, permissions and timestamps behave
// like the backing ext4. Files whose names match the chunk globs (llama.cpp
// slot snapshots) are stored as stubs instead: the stub in the tree holds the
// file's chunk index (casync .caibx format) and an xattr with its logical size;
// the data lives as content-defined chunks in <backing>/store. Because the stub
// is the file in the tree, rename and unlink stay atomic and cost nothing.
//
// Writes: a write handle feeds bytes to a streaming chunker while they arrive in
// order, so only chunks new to the store are written. An append keeps the
// file's existing chunks and streams only the new bytes: llama-server writes a
// snapshot in two passes, the state (create, truncate) and then the checkpoint
// section (reopen with O_APPEND). Anything else - a write out of order, a read
// of data still streaming, a partial truncate - switches the handle to a
// staging copy in <backing>/staging, which is chunked when the handle is flushed.
package kvfs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/folbricht/desync"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"kvchunkfs/internal/chunks"
	"kvchunkfs/internal/events"
)

// sizeXattr marks a stub and holds its file's logical size in bytes.
const sizeXattr = "user.kvchunkfs.size"

// Options configure the filesystem.
type Options struct {
	Backing    string   // holds tree/ (what the mount shows) and staging/
	ChunkGlobs []string // base names stored as chunks, e.g. "*.bin"
	Readahead  int64    // bytes prefetched past a sequential reader
	Mountpoint string   // for the status file
	Version    string   // for the status file
	Events     *events.Log  // where log lines and events go (nil: stderr only)
	APIAddrs   []string     // the API's listeners, for the status file
	Score      ScoreWeights // eviction score of snapshots (see Snapshots)
	UseWindow  time.Duration // reads of a snapshot within this window are one use
}

// FS is the shared state of one mount.
type FS struct {
	store   *chunks.Store
	opt     Options
	tree    string
	staging string
	started time.Time
	ev      *events.Log
	m       *fsMetrics

	mu          sync.Mutex
	pinned      map[any][]desync.IndexChunk // chunks open files need (see pin)
	lastGC      GCResult
	errors      int
	lastErr     string
	parsedCache map[string]*parsed // index and header per stub version (snapshots.go)
}

// NewRoot prepares <backing>/tree and <backing>/staging and returns the root node.
func NewRoot(store *chunks.Store, opt Options) (*Node, error) {
	k := &FS{
		store:   store,
		opt:     opt,
		started: time.Now(),
		tree:    filepath.Join(opt.Backing, "tree"),
		staging: filepath.Join(opt.Backing, "staging"),
		ev:      opt.Events,
	}
	if k.ev == nil {
		k.ev = events.New(0, os.Stderr)
	}
	k.m = newMetrics(k)
	for _, d := range []string{k.tree, k.staging} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	// a crash leaves staging copies behind; nothing refers to them
	if old, _ := filepath.Glob(filepath.Join(k.staging, "*")); len(old) > 0 {
		for _, p := range old {
			_ = os.Remove(p)
		}
		k.ev.Infof("removed %d leftover staging files", len(old))
	}
	var st syscall.Stat_t
	if err := syscall.Stat(k.tree, &st); err != nil {
		return nil, err
	}
	lr := &fs.LoopbackRoot{Path: k.tree, Dev: uint64(st.Dev)}
	lr.NewNode = func(r *fs.LoopbackRoot, _ *fs.Inode, _ string, _ *syscall.Stat_t) fs.InodeEmbedder {
		return &Node{LoopbackNode: fs.LoopbackNode{RootData: r}, kfs: k}
	}
	root := &Node{LoopbackNode: fs.LoopbackNode{RootData: lr}, kfs: k}
	lr.RootNode = root
	return root, nil
}

// FS returns the mount's shared state (garbage collection, status).
func (n *Node) FS() *FS { return n.kfs }

func (k *FS) chunked(name string) bool {
	for _, g := range k.opt.ChunkGlobs {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	return false
}

// stubSize returns the logical size of the stub at path, or false for a plain file.
func stubSize(path string) (int64, bool) {
	buf := make([]byte, 24)
	n, err := syscall.Getxattr(path, sizeXattr, buf)
	if err != nil {
		return 0, false
	}
	size, err := strconv.ParseInt(string(buf[:n]), 10, 64)
	return size, err == nil
}

// openBlob reads the index in the stub at path.
func (k *FS) openBlob(path string, size int64) (*chunks.File, error) {
	if size == 0 {
		return chunks.NewFile(k.store, nil), nil
	}
	return chunks.OpenIndex(k.store, path)
}

// writeStub atomically replaces the stub at path with idx, keeping its mode.
func (k *FS) writeStub(path string, idx desync.Index) error {
	tmp, err := os.CreateTemp(k.staging, "stub-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if len(idx.Chunks) > 0 {
		if _, err := idx.WriteTo(tmp); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := syscall.Setxattr(tmp.Name(), sizeXattr, []byte(strconv.FormatInt(chunks.IndexSize(idx), 10)), 0); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmp.Name(), st.Mode().Perm())
		copyMeta(path, tmp.Name()) // uses, created, labels survive the rewrite
	}
	return os.Rename(tmp.Name(), path)
}

// Metrics writes the Prometheus metrics.
func (k *FS) Metrics(w io.Writer) { k.m.reg.Render(w) }

// Events is the mount's event log.
func (k *FS) Events() *events.Log { return k.ev }

// Collect runs a garbage collection now (the API's POST /gc).
func (k *FS) Collect() GCResult { return k.GC(time.Minute) }

// Node is a file or directory in the mount.
type Node struct {
	fs.LoopbackNode
	kfs  *FS
	wmu  sync.Mutex
	open map[*writeHandle]struct{} // write handles open on this file
}

var (
	_ = (fs.NodeOnAdder)((*Node)(nil))
	_ = (fs.NodeReaddirer)((*Node)(nil))
	_ = (fs.NodeLookuper)((*Node)(nil))
	_ = (fs.NodeGetattrer)((*Node)(nil))
	_ = (fs.NodeSetattrer)((*Node)(nil))
	_ = (fs.NodeCreater)((*Node)(nil))
	_ = (fs.NodeOpener)((*Node)(nil))
	_ = (fs.NodeStatxer)((*Node)(nil))
	_ = (fs.NodeCopyFileRanger)((*Node)(nil))
)

// backing returns the node's path in the tree, or "" once it is unlinked.
func (n *Node) backing() string {
	if n.Root() != n.EmbeddedInode() {
		if name, parent := n.Parent(); parent == nil || name == "" {
			return ""
		}
	}
	return filepath.Join(n.kfs.tree, n.Path(n.Root()))
}

func fixSize(a *fuse.Attr, size int64) {
	a.Size = uint64(size)
	a.Blocks = (a.Size + 511) / 512
}

func (n *Node) isRoot() bool { return n.Root() == n.EmbeddedInode() }

// OnAdd puts the virtual status file into the root.
func (n *Node) OnAdd(ctx context.Context) {
	if n.isRoot() {
		st := n.NewPersistentInode(ctx, &statusNode{kfs: n.kfs}, fs.StableAttr{Mode: syscall.S_IFREG})
		n.AddChild(StatusName, st, true)
	}
}

// Readdir lists the root with the status file in it.
func (n *Node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	ds, errno := n.LoopbackNode.Readdir(ctx)
	if errno != 0 || !n.isRoot() {
		return ds, errno
	}
	entries := []fuse.DirEntry{{Name: StatusName, Mode: syscall.S_IFREG}}
	for ds.HasNext() {
		e, errno := ds.Next()
		if errno != 0 {
			break
		}
		if e.Name != StatusName {
			entries = append(entries, e)
		}
	}
	ds.Close()
	return fs.NewListDirStream(entries), 0
}

// OpendirHandle: the loopback lists directories through its own handle, which
// would leave out the status file, so the root gets a list of its own.
func (n *Node) OpendirHandle(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if !n.isRoot() {
		return n.LoopbackNode.OpendirHandle(ctx, flags)
	}
	ds, errno := n.Readdir(ctx)
	if errno != 0 {
		return nil, 0, errno
	}
	d := &listDir{}
	for ds.HasNext() {
		e, errno := ds.Next()
		if errno != 0 {
			return nil, 0, errno
		}
		d.entries = append(d.entries, e)
	}
	return d, 0, 0
}

// listDir is a directory handle over a fixed list of entries.
type listDir struct {
	entries []fuse.DirEntry
	next    int
}

var (
	_ = (fs.FileReaddirenter)((*listDir)(nil))
	_ = (fs.FileSeekdirer)((*listDir)(nil))
)

func (d *listDir) Readdirent(ctx context.Context) (*fuse.DirEntry, syscall.Errno) {
	if d.next >= len(d.entries) {
		return nil, 0
	}
	e := d.entries[d.next]
	d.next++
	return &e, 0
}

func (d *listDir) Seekdir(ctx context.Context, off uint64) syscall.Errno {
	d.next = int(off)
	return 0
}

// Lookup reports a stub with its logical size; in the root, "status" is ours.
func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.isRoot() && name == StatusName {
		ch := n.GetChild(StatusName)
		var a fuse.AttrOut
		ch.Operations().(*statusNode).Getattr(ctx, nil, &a)
		out.Attr = a.Attr
		return ch, 0
	}
	ch, errno := n.LoopbackNode.Lookup(ctx, name, out)
	if errno == 0 && out.Mode&syscall.S_IFMT == syscall.S_IFREG {
		if size, ok := stubSize(filepath.Join(n.backing(), name)); ok {
			fixSize(&out.Attr, size)
		}
	}
	return ch, errno
}

// Getattr reports a stub's logical size, or an open writer's current size.
func (n *Node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if errno := n.LoopbackNode.Getattr(ctx, loopbackOnly(f), out); errno != 0 {
		return errno
	}
	if h, ok := f.(*writeHandle); ok {
		fixSize(&out.Attr, h.size())
	} else if out.Mode&syscall.S_IFMT == syscall.S_IFREG {
		if size, ok := stubSize(n.backing()); ok {
			fixSize(&out.Attr, size)
		}
	}
	return 0
}

// loopbackOnly passes the loopback's own file handles on, and hides ours from it.
func loopbackOnly(f fs.FileHandle) fs.FileHandle {
	switch f.(type) {
	case *writeHandle, *readHandle:
		return nil
	}
	return f
}

func isOurs(f fs.FileHandle) bool { return loopbackOnly(f) == nil && f != nil }

// Setattr handles a size change of a stub itself; everything else (mode,
// owner, times: the proxy's LRU touch) goes to the stub in the tree.
func (n *Node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	path := n.backing()
	_, stub := stubSize(path)
	if size, ok := in.GetSize(); ok && (stub || isOurs(f)) {
		var errno syscall.Errno
		if h, ok := f.(*writeHandle); ok {
			errno = h.truncate(int64(size))
		} else {
			errno = n.truncateStub(ctx, path, int64(size))
		}
		if errno != 0 {
			return errno
		}
		in.Valid &^= fuse.FATTR_SIZE
	}
	if errno := n.LoopbackNode.Setattr(ctx, loopbackOnly(f), in, out); errno != 0 {
		return errno
	}
	return n.Getattr(ctx, f, out)
}

// truncateStub resizes a stub by path (truncate(2), or the kernel's own
// truncate after an open when O_TRUNC is not passed with it). Handles open for
// writing hold their own view of the content, so they are truncated too;
// otherwise their next flush would write the old content back.
func (n *Node) truncateStub(ctx context.Context, path string, size int64) syscall.Errno {
	open := n.writers()
	for _, h := range open {
		if errno := h.truncate(size); errno != 0 {
			return errno
		}
	}
	if size == 0 {
		return fs.ToErrno(n.kfs.writeStub(path, desync.Index{}))
	}
	if len(open) > 0 {
		return 0 // the stub follows when they flush
	}
	h := n.newHandle()
	defer h.Release(ctx)
	if errno := h.truncate(size); errno != 0 {
		return errno
	}
	return h.Flush(ctx)
}

// Create makes a new file; a chunked one starts as an empty stub with a
// streaming write handle.
func (n *Node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	if !n.kfs.chunked(name) {
		return n.LoopbackNode.Create(ctx, name, flags, mode, out)
	}
	path := filepath.Join(n.backing(), name)
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_WRONLY|syscall.O_TRUNC|int(flags&syscall.O_EXCL), mode)
	if err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	syscall.Close(fd)
	if err := syscall.Setxattr(path, sizeXattr, []byte("0"), 0); err != nil {
		_ = os.Remove(path)
		return nil, nil, 0, fs.ToErrno(err)
	}
	_ = syscall.Setxattr(path, createdXattr, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0)
	ch, errno := n.Lookup(ctx, name, out)
	if errno != 0 {
		return nil, nil, 0, errno
	}
	h := ch.Operations().(*Node).newHandle()
	h.startStream(nil, 0, false)
	return ch, h, 0, 0
}

// Open opens a stub through our handles, anything else through the loopback.
// A write handle starts idle - the content is the stub - and decides on its
// first operation: O_TRUNC or a truncate to zero starts a stream, a write at
// the end streams an append onto the existing chunks, anything else stages a
// copy. So opening costs nothing, and llama-server's two passes (create, then
// reopen with O_APPEND for the checkpoints) both stream.
func (n *Node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	path := n.backing()
	size, stub := stubSize(path)
	if !stub {
		return n.LoopbackNode.Open(ctx, flags)
	}
	if flags&syscall.O_ACCMODE == syscall.O_RDONLY {
		f, err := n.kfs.openBlob(path, size)
		if err != nil {
			n.kfs.fail("open %s: %v", path, err)
			return nil, 0, syscall.EIO
		}
		h := &readHandle{kfs: n.kfs, node: n, r: f.NewReader(n.kfs.opt.Readahead), opened: time.Now()}
		n.kfs.pin(h, f.Chunks()) // readable even if the stub is deleted meanwhile
		return h, 0, 0
	}
	h := n.newHandle()
	if flags&syscall.O_TRUNC != 0 {
		h.startStream(nil, 0, true)
	}
	return h, 0, 0
}

// Statx is declined, so the kernel falls back to Getattr, which knows stubs.
func (n *Node) Statx(ctx context.Context, f fs.FileHandle, flags uint32, mask uint32, out *fuse.StatxOut) syscall.Errno {
	return syscall.ENOSYS
}

// CopyFileRange is declined for our handles (the kernel then copies through
// read and write); loopback files keep the fast path.
func (n *Node) CopyFileRange(ctx context.Context, fhIn fs.FileHandle, offIn uint64, out *fs.Inode, fhOut fs.FileHandle, offOut uint64, length uint64, flags uint64) (uint32, syscall.Errno) {
	if isOurs(fhIn) || isOurs(fhOut) {
		return 0, syscall.ENOSYS
	}
	return n.LoopbackNode.CopyFileRange(ctx, fhIn, offIn, out, fhOut, offOut, length, flags)
}

// readHandle reads a stub's chunks.
type readHandle struct {
	kfs    *FS
	node   *Node
	r      *chunks.Reader
	opened time.Time
	read   atomic.Int64
}

var (
	_ = (fs.FileReader)((*readHandle)(nil))
	_ = (fs.FileReleaser)((*readHandle)(nil))
)

func (h *readHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.r.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		h.kfs.fail("read at %d: %v", off, err)
		return nil, syscall.EIO
	}
	h.read.Add(int64(n))
	return fuse.ReadResultData(dest[:n]), 0
}

// Release ends a read. A read of at least 1 MiB (or half the file) is a use of
// the snapshot, counted at most once per UseWindow: llama-server restores a
// snapshot in several opens within a second (a ~20 KB header and token check,
// then the state, and for some models the appended context checkpoints - each open also
// touching the end of the file), which is one restore. Header checks are read
// events, not uses.
func (h *readHandle) Release(ctx context.Context) syscall.Errno {
	h.kfs.unpin(h)
	if n := h.read.Load(); n > 0 {
		secs := time.Since(h.opened).Seconds()
		path := h.node.backing()
		if path != "" && (n >= 1<<20 || n >= h.r.Size()/2) {
			countUse(path, h.kfs.opt.UseWindow)
		}
		rel := h.kfs.rel(path)
		model := modelOf(rel)
		h.kfs.m.reads.Inc(model)
		h.kfs.m.readBytes.Add(float64(n), model)
		h.kfs.m.readSeconds.Observe(secs, model)
		h.kfs.ev.Emit("info", "read", "read", map[string]any{
			"model": model, "file": rel, "bytes": n, "size": h.r.Size(), "seconds": round3(secs)})
	}
	return 0
}

func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }

// writeHandle is a stub opened for writing. It is in one of three states:
//   - idle: no stream, no staging copy; the content is the stub on disk
//   - streaming: bytes arrive in order and are chunked as they come; base holds
//     the chunks an append continues (nil for a fresh stream)
//   - staging: a full copy in <backing>/staging, chunked at flush
//
// Flush turns a streaming or staging handle into the stub's new index and
// leaves it idle, so a dup'd descriptor can keep writing.
type writeHandle struct {
	mu       sync.Mutex
	opened   time.Time // start of the current write, for its event
	node     *Node
	base     []desync.IndexChunk
	baseSize int64
	stream   *chunks.StreamWriter
	staging  *os.File
	dirty    bool // changed since the last flush
}

var (
	_ = (fs.FileWriter)((*writeHandle)(nil))
	_ = (fs.FileReader)((*writeHandle)(nil))
	_ = (fs.FileFlusher)((*writeHandle)(nil))
	_ = (fs.FileFsyncer)((*writeHandle)(nil))
	_ = (fs.FileReleaser)((*writeHandle)(nil))
)

// newHandle returns an idle write handle registered on its node.
func (n *Node) newHandle() *writeHandle {
	h := &writeHandle{node: n, opened: time.Now()}
	n.wmu.Lock()
	if n.open == nil {
		n.open = map[*writeHandle]struct{}{}
	}
	n.open[h] = struct{}{}
	n.wmu.Unlock()
	return h
}

// writers are the node's write handles that are open now.
func (n *Node) writers() []*writeHandle {
	n.wmu.Lock()
	defer n.wmu.Unlock()
	out := make([]*writeHandle, 0, len(n.open))
	for h := range n.open {
		out = append(out, h)
	}
	return out
}

// startStream starts chunking from baseSize on. dirty says whether the content
// already differs from the stub (a truncate) or only once data arrives (a new
// file, whose empty stub Create wrote; an O_TRUNC open).
func (h *writeHandle) startStream(base []desync.IndexChunk, baseSize int64, dirty bool) {
	h.stream = h.node.kfs.store.NewStreamWriter(context.Background())
	h.base, h.baseSize = base, baseSize
	h.dirty = dirty
	if base != nil {
		h.node.kfs.pin(h, base) // the append continues these, whatever happens to the stub
	}
}

// current is the stub on disk, for an idle handle.
func (h *writeHandle) current() (*chunks.File, syscall.Errno) {
	path := h.node.backing()
	size, _ := stubSize(path)
	blob, err := h.node.kfs.openBlob(path, size)
	if err != nil {
		h.node.kfs.fail("open %s: %v", path, err)
		return nil, syscall.EIO
	}
	return blob, 0
}

func (h *writeHandle) size() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.stream != nil:
		return h.baseSize + h.stream.Written()
	case h.staging != nil:
		if st, err := h.staging.Stat(); err == nil {
			return st.Size()
		}
	}
	size, _ := stubSize(h.node.backing())
	return size
}

// stage switches to a staging copy of the current content.
func (h *writeHandle) stage() syscall.Errno {
	var blob *chunks.File
	switch {
	case h.staging != nil:
		return 0
	case h.stream != nil:
		idx, _, err := h.stream.Close()
		h.stream = nil
		if err != nil {
			h.node.kfs.fail("stream: %v", err)
			return syscall.EIO
		}
		blob = chunks.NewFile(h.node.kfs.store, chunks.Append(h.base, idx.Chunks))
		defer h.node.kfs.unpin(h) // once the staging copy holds the content
		h.base, h.baseSize = nil, 0
	default:
		var errno syscall.Errno
		if blob, errno = h.current(); errno != 0 {
			return errno
		}
	}
	f, err := os.CreateTemp(h.node.kfs.staging, "data-*")
	if err != nil {
		return fs.ToErrno(err)
	}
	if _, err := io.Copy(f, io.NewSectionReader(blob, 0, blob.Size())); err != nil {
		f.Close()
		os.Remove(f.Name())
		h.node.kfs.fail("staging copy: %v", err)
		return syscall.EIO
	}
	h.staging = f
	return 0
}

func (h *writeHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stream == nil && h.staging == nil {
		// idle: a write at the end appends to the existing chunks
		blob, errno := h.current()
		if errno != 0 {
			return 0, errno
		}
		if off == blob.Size() {
			h.startStream(blob.Chunks(), blob.Size(), false)
		}
	}
	if h.stream != nil && off == h.baseSize+h.stream.Written() {
		h.dirty = true
		n, err := h.stream.Write(data)
		if err != nil {
			h.node.kfs.fail("stream write: %v", err)
			return uint32(n), syscall.EIO
		}
		return uint32(n), 0
	}
	if errno := h.stage(); errno != 0 {
		return 0, errno
	}
	h.dirty = true
	n, err := h.staging.WriteAt(data, off)
	return uint32(n), fs.ToErrno(err)
}

func (h *writeHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int
	var err error
	if h.stream == nil && h.staging == nil {
		blob, errno := h.current()
		if errno != 0 {
			return nil, errno
		}
		n, err = blob.ReadAt(dest, off)
	} else {
		if errno := h.stage(); errno != 0 {
			return nil, errno
		}
		n, err = h.staging.ReadAt(dest, off)
	}
	if err != nil && err != io.EOF {
		return nil, fs.ToErrno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *writeHandle) truncate(size int64) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if size == 0 {
		// nothing to keep: a fresh stream, whatever the state was
		if h.stream != nil {
			_, _, _ = h.stream.Close()
		}
		if h.staging != nil {
			h.staging.Close()
			os.Remove(h.staging.Name())
			h.staging = nil
		}
		h.startStream(nil, 0, true)
		return 0
	}
	if h.stream != nil && size == h.baseSize+h.stream.Written() {
		return 0
	}
	if h.stream == nil && h.staging == nil {
		if cur, _ := stubSize(h.node.backing()); cur == size {
			return 0
		}
	}
	if errno := h.stage(); errno != 0 {
		return errno
	}
	h.dirty = true
	return fs.ToErrno(h.staging.Truncate(size))
}

// Flush writes the stub's new index. close() returns only after this, so the
// file is complete before the writer goes on (the proxy renames it next).
func (h *writeHandle) Flush(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.finish()
}

func (h *writeHandle) finish() syscall.Errno {
	if !h.dirty {
		return 0
	}
	start := time.Now()
	var idx desync.Index
	var stats chunks.WriteStats
	var err error
	mode := "staging"
	switch {
	case h.stream != nil:
		mode = "stream"
		if h.base != nil {
			mode = "append"
		}
		idx, stats, err = h.stream.Close()
		h.stream = nil
		if h.base != nil {
			idx.Chunks = chunks.Append(h.base, idx.Chunks)
			stats.Chunks, stats.Bytes = int64(len(idx.Chunks)), chunks.IndexSize(idx)
			// stays pinned until the new stub, which references them, is written
			defer h.node.kfs.unpin(h)
		}
		h.base, h.baseSize = nil, 0
	case h.staging != nil:
		var st os.FileInfo
		if st, err = h.staging.Stat(); err == nil {
			idx, stats, err = h.node.kfs.store.ChunkReader(context.Background(), io.NewSectionReader(h.staging, 0, st.Size()))
		}
		h.staging.Close()
		os.Remove(h.staging.Name())
		h.staging = nil
	}
	h.dirty = false
	if err != nil {
		h.node.kfs.fail("chunking: %v", err)
		return syscall.EIO
	}
	path := h.node.backing()
	if path == "" {
		return 0 // unlinked while open: nothing to keep
	}
	if err := h.node.kfs.writeStub(path, idx); err != nil {
		h.node.kfs.fail("stub %s: %v", path, err)
		return fs.ToErrno(err)
	}
	rel := h.node.kfs.rel(path)
	model := modelOf(rel)
	secs := time.Since(h.opened).Seconds()
	m := h.node.kfs.m
	m.writes.Inc(model)
	m.writeBytes.Add(float64(stats.Bytes), model)
	m.writeNewBytes.Add(float64(stats.NewBytes), model)
	m.writeSeconds.Observe(secs, model)
	h.node.kfs.ev.Emit("info", "write", "saved", map[string]any{
		"model": model, "file": rel, "mode": mode, "bytes": stats.Bytes, "chunks": stats.Chunks,
		"new_chunks": stats.NewChunks, "new_bytes": stats.NewBytes, "seconds": round3(secs),
		"finish_ms": time.Since(start).Milliseconds()})
	h.opened = time.Now()
	return 0
}

// Fsync flushes a staging copy; a stream's data is complete only at Flush.
func (h *writeHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.staging != nil {
		return fs.ToErrno(h.staging.Sync())
	}
	return 0
}

// Release finishes anything a missing Flush left open and unregisters.
func (h *writeHandle) Release(ctx context.Context) syscall.Errno {
	defer h.node.kfs.unpin(h)
	h.node.wmu.Lock()
	delete(h.node.open, h)
	h.node.wmu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	errno := h.finish()
	if h.staging != nil {
		h.staging.Close()
		os.Remove(h.staging.Name())
		h.staging = nil
	}
	return errno
}
