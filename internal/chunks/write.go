package chunks

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/folbricht/desync"
)

// WriteStats count what writes added to the store.
type WriteStats struct {
	Chunks, NewChunks, Bytes, NewBytes int64
}

// dedupStore counts what is stored. desync's ChunkStorage (inside ChunkStream)
// already skips chunks the store has, so StoreChunk is only called for new ones.
type dedupStore struct {
	*Store
	chunks, newChunks, bytes, newBytes atomic.Int64
}

func (d *dedupStore) GetChunk(id desync.ChunkID) (*desync.Chunk, error) { return d.ls.GetChunk(id) }
func (d *dedupStore) HasChunk(id desync.ChunkID) (bool, error)         { return d.ls.HasChunk(id) }
func (d *dedupStore) Close() error                                     { return nil }
func (d *dedupStore) String() string                                   { return d.ls.String() }

func (d *dedupStore) StoreChunk(c *desync.Chunk) error {
	data, err := c.Data()
	if err != nil {
		return err
	}
	if err := d.ls.StoreChunk(c); err != nil {
		return err
	}
	d.newChunks.Add(1)
	d.newBytes.Add(int64(len(data)))
	return nil
}

func (d *dedupStore) stats() WriteStats {
	return WriteStats{d.chunks.Load(), d.newChunks.Load(), d.bytes.Load(), d.newBytes.Load()}
}

// ChunkReader chunks everything r yields into the store and returns the index.
func (s *Store) ChunkReader(ctx context.Context, r io.Reader) (desync.Index, WriteStats, error) {
	s.gc.RLock() // no garbage collection while this may reuse existing chunks
	defer s.gc.RUnlock()
	ds := &dedupStore{Store: s}
	chunker, err := desync.NewChunker(r, s.sizes.Min, s.sizes.Avg, s.sizes.Max)
	if err != nil {
		return desync.Index{}, WriteStats{}, err
	}
	idx, err := desync.ChunkStream(ctx, chunker, ds, s.workers)
	st := ds.stats()
	// ChunkStream skips chunks the store has, so count totals from the index
	st.Chunks, st.Bytes = int64(len(idx.Chunks)), IndexSize(idx)
	return idx, st, err
}

// Append returns base followed by more, with more's offsets moved past base.
func Append(base, more []desync.IndexChunk) []desync.IndexChunk {
	var off uint64
	if n := len(base); n > 0 {
		off = base[n-1].Start + base[n-1].Size
	}
	out := make([]desync.IndexChunk, 0, len(base)+len(more))
	out = append(out, base...)
	for _, c := range more {
		c.Start += off
		out = append(out, c)
	}
	return out
}

// StreamWriter chunks bytes as they are written, in order, so a sequential
// writer never needs a full staging copy: only chunks new to the store reach
// the disk. Writes are copied into a bounded queue (streamQueue buffers) and
// return at once, so the writer and the chunker run at the same time; a
// writer that gets ahead of the chunker blocks until there is room.
type StreamWriter struct {
	q       chan []byte
	done    chan struct{}
	idx     desync.Index
	stats   WriteStats
	err     error
	written int64
}

// streamQueue bounds the bytes in flight per writer: 32 x 1 MiB FUSE writes.
const streamQueue = 32

// NewStreamWriter starts a chunker reading from the returned writer.
func (s *Store) NewStreamWriter(ctx context.Context) *StreamWriter {
	w := &StreamWriter{q: make(chan []byte, streamQueue), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		r := &queueReader{q: w.q}
		w.idx, w.stats, w.err = s.ChunkReader(ctx, r)
		for range w.q { // drain, so a writer is never stuck after a failure
		}
	}()
	return w
}

// Write queues a copy of p (the caller may reuse p).
func (w *StreamWriter) Write(p []byte) (int, error) {
	select {
	case <-w.done:
		if w.err != nil {
			return 0, w.err
		}
		return 0, io.ErrClosedPipe
	default:
	}
	w.q <- append([]byte(nil), p...)
	w.written += int64(len(p))
	return len(p), nil
}

// Written is the number of bytes accepted so far.
func (w *StreamWriter) Written() int64 { return w.written }

// Close ends the stream and returns the index of everything written.
func (w *StreamWriter) Close() (desync.Index, WriteStats, error) {
	close(w.q)
	<-w.done
	return w.idx, w.stats, w.err
}

// queueReader is the chunker's side of a StreamWriter's queue.
type queueReader struct {
	q   chan []byte
	cur []byte
}

func (r *queueReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		b, ok := <-r.q
		if !ok {
			return 0, io.EOF
		}
		r.cur = b
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// WriteIndex writes idx to path atomically.
func WriteIndex(idx desync.Index, path string) error { return writeIndex(idx, path) }

// IndexSize is the length of the blob an index describes.
func IndexSize(idx desync.Index) int64 {
	if n := len(idx.Chunks); n > 0 {
		return int64(idx.Chunks[n-1].Start + idx.Chunks[n-1].Size)
	}
	return 0
}
