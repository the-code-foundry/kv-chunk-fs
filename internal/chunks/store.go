// Package chunks reads content-defined chunks from a desync local store, caches
// decoded chunks in a byte-bounded LRU and prefetches ahead of sequential readers.
package chunks

import (
	"container/list"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/folbricht/desync"
)

// Options configure a Store.
type Options struct {
	Compressed bool  // chunks are zstd-compressed (desync's default store format)
	Verify     bool  // check every chunk's hash when it is read from disk
	CacheBytes int64 // upper bound for decoded chunks kept in memory
	Workers    int   // prefetch goroutines, and goroutines storing chunks on writes
	Sizes      ChunkSizes // chunker sizes for writes; zero means DefaultChunkSizes
}

// Stats are counters for reporting; read them with Store.Stats.
type Stats struct {
	Hits, Misses, Fetched, FetchedBytes, Prefetched, PrefetchDropped int64
}

// Store hands out decoded chunks by ID.
type Store struct {
	gc      sync.RWMutex // shared by writers, exclusive for garbage collection
	ls      desync.LocalStore
	sizes   ChunkSizes
	workers int
	cache  *lru
	mu     sync.Mutex
	flight map[desync.ChunkID]*call
	queue  chan desync.ChunkID
	stats  struct {
		hits, misses, fetched, fetchedBytes, prefetched, prefetchDropped atomic.Int64
	}
}

type call struct {
	done chan struct{}
	data []byte
	err  error
}

// Open opens the store in dir and starts the prefetch workers.
func Open(dir string, opt Options) (*Store, error) {
	ls, err := desync.NewLocalStore(dir, desync.StoreOptions{
		Uncompressed: !opt.Compressed,
		SkipVerify:   !opt.Verify,
	})
	if err != nil {
		return nil, fmt.Errorf("store %s: %w", dir, err)
	}
	if opt.Workers < 1 {
		opt.Workers = 1
	}
	if opt.Sizes == (ChunkSizes{}) {
		opt.Sizes = DefaultChunkSizes
	}
	s := &Store{
		ls:      ls,
		sizes:   opt.Sizes,
		workers: opt.Workers,
		cache:  newLRU(opt.CacheBytes),
		flight: map[desync.ChunkID]*call{},
		queue:  make(chan desync.ChunkID, 4096),
	}
	for range opt.Workers {
		go func() {
			for id := range s.queue {
				_, _ = s.Get(id)
			}
		}()
	}
	return s, nil
}

// Get returns a chunk's decoded bytes. Concurrent calls for the same chunk read
// it from disk once. The returned slice is shared and must not be modified.
func (s *Store) Get(id desync.ChunkID) ([]byte, error) {
	if b, ok := s.cache.get(id); ok {
		s.stats.hits.Add(1)
		return b, nil
	}
	s.mu.Lock()
	if c, ok := s.flight[id]; ok {
		s.mu.Unlock()
		<-c.done
		s.stats.hits.Add(1)
		return c.data, c.err
	}
	c := &call{done: make(chan struct{})}
	s.flight[id] = c
	s.mu.Unlock()
	s.stats.misses.Add(1)

	c.data, c.err = s.fetch(id)
	if c.err == nil {
		s.cache.put(id, c.data)
	}
	s.mu.Lock()
	delete(s.flight, id)
	s.mu.Unlock()
	close(c.done)
	return c.data, c.err
}

func (s *Store) fetch(id desync.ChunkID) ([]byte, error) {
	chunk, err := s.ls.GetChunk(id)
	if err != nil {
		return nil, err
	}
	b, err := chunk.Data()
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %w", id, err)
	}
	s.stats.fetched.Add(1)
	s.stats.fetchedBytes.Add(int64(len(b)))
	return b, nil
}

// Prefetch queues a chunk to be read into the cache in the background. It never
// blocks: if the queue is full, or the chunk is cached already, it does nothing.
func (s *Store) Prefetch(id desync.ChunkID) {
	if s.cache.has(id) {
		return
	}
	select {
	case s.queue <- id:
		s.stats.prefetched.Add(1)
	default:
		s.stats.prefetchDropped.Add(1)
	}
}

// Stats returns the counters so far.
func (s *Store) Stats() Stats {
	return Stats{
		Hits: s.stats.hits.Load(), Misses: s.stats.misses.Load(),
		Fetched: s.stats.fetched.Load(), FetchedBytes: s.stats.fetchedBytes.Load(),
		Prefetched: s.stats.prefetched.Load(), PrefetchDropped: s.stats.prefetchDropped.Load(),
	}
}

// lru is a byte-bounded least-recently-used cache of decoded chunks.
type lru struct {
	mu    sync.Mutex
	max   int64
	size  int64
	order *list.List // front = most recent
	items map[desync.ChunkID]*list.Element
}

type entry struct {
	id   desync.ChunkID
	data []byte
}

func newLRU(max int64) *lru {
	return &lru{max: max, order: list.New(), items: map[desync.ChunkID]*list.Element{}}
}

func (l *lru) get(id desync.ChunkID) ([]byte, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.items[id]; ok {
		l.order.MoveToFront(e)
		return e.Value.(*entry).data, true
	}
	return nil, false
}

func (l *lru) has(id desync.ChunkID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.items[id]
	return ok
}

func (l *lru) put(id desync.ChunkID, data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.items[id]; ok {
		return
	}
	l.items[id] = l.order.PushFront(&entry{id, data})
	l.size += int64(len(data))
	for l.size > l.max && l.order.Len() > 1 {
		e := l.order.Back()
		old := e.Value.(*entry)
		l.order.Remove(e)
		delete(l.items, old.id)
		l.size -= int64(len(old.data))
	}
}
