package chunks

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/folbricht/desync"
)

// File is one blob: an index of chunks in a Store.
type File struct {
	store  *Store
	chunks []desync.IndexChunk
	size   int64
}

// OpenIndex reads a .caibx index and returns the blob it describes.
func OpenIndex(store *Store, path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	idx, err := desync.IndexFromReader(f)
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", path, err)
	}
	return NewFile(store, idx.Chunks), nil
}

// NewFile returns the blob made of these chunks, in order.
func NewFile(store *Store, chunks []desync.IndexChunk) *File {
	var size int64
	if n := len(chunks); n > 0 {
		size = int64(chunks[n-1].Start + chunks[n-1].Size)
	}
	return &File{store: store, chunks: chunks, size: size}
}

// Size is the blob's length in bytes.
func (f *File) Size() int64 { return f.size }

// Chunks is the blob's index.
func (f *File) Chunks() []desync.IndexChunk { return f.chunks }

// chunkAt returns the index of the chunk containing offset off.
func (f *File) chunkAt(off int64) int {
	return sort.Search(len(f.chunks), func(i int) bool {
		return int64(f.chunks[i].Start+f.chunks[i].Size) > off
	})
}

// ReadAt fills p from offset off, like io.ReaderAt.
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.size {
		return 0, io.EOF
	}
	n := 0
	for i := f.chunkAt(off); n < len(p) && i < len(f.chunks); i++ {
		c := f.chunks[i]
		data, err := f.store.Get(c.ID)
		if err != nil {
			return n, err
		}
		if uint64(len(data)) != c.Size {
			return n, fmt.Errorf("chunk %s: %d bytes, index says %d", c.ID, len(data), c.Size)
		}
		n += copy(p[n:], data[off+int64(n)-int64(c.Start):])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Reader is one open handle on a File. It detects sequential reads and keeps
// the next window of chunks prefetching while the current one is served.
type Reader struct {
	f         *File
	readahead int64
	mu        sync.Mutex
	next      int64 // where a sequential reader reads next
	queued    int   // chunks before this index have been queued or read
}

// NewReader returns a handle that prefetches readahead bytes past sequential reads.
func (f *File) NewReader(readahead int64) *Reader {
	return &Reader{f: f, readahead: readahead}
}

// ReadAt reads like File.ReadAt and schedules read-ahead when the access is
// sequential (the kernel splits one large read into consecutive requests, which
// may arrive slightly out of order, so "close to the last end" counts too).
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	sequential := off == 0 || (off >= r.next-int64(len(p))*4 && off <= r.next+int64(len(p))*4)
	if sequential && r.readahead > 0 {
		end := off + int64(len(p)) + r.readahead
		first := r.f.chunkAt(off + int64(len(p)))
		if first < r.queued {
			first = r.queued
		}
		i := first
		for ; i < len(r.f.chunks) && int64(r.f.chunks[i].Start) < end; i++ {
			r.f.store.Prefetch(r.f.chunks[i].ID)
		}
		if i > r.queued {
			r.queued = i
		}
	} else {
		r.queued = 0 // random access: start a fresh window at the next sequential run
	}
	r.mu.Unlock()

	n, err := r.f.ReadAt(p, off)

	r.mu.Lock()
	if off+int64(n) > r.next || !sequential {
		r.next = off + int64(n)
	}
	r.mu.Unlock()
	return n, err
}

// Size is the blob's length in bytes.
func (r *Reader) Size() int64 { return r.f.size }
