package chunks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/folbricht/desync"
)

// Writers hold the store's gc lock shared for as long as they add chunks;
// garbage collection holds it exclusively. So a collection never runs while a
// writer may have decided to reuse a chunk that nothing references yet (an
// orphan of a deleted snapshot), and no grace period is needed.

// TryExclusive runs fn if no writer is adding chunks right now, and reports
// whether it did. It never waits, so a writer that keeps a file open does not
// make new writers queue up behind a waiting collection.
func (s *Store) TryExclusive(fn func() error) bool {
	if !s.gc.TryLock() {
		return false
	}
	defer s.gc.Unlock()
	_ = fn()
	return true
}

// ChunkInfo is one chunk file in the store.
type ChunkInfo struct {
	ID   desync.ChunkID
	Path string
	Size int64
}

// List returns every chunk in the store and removes leftover temporary files
// (chunk writes interrupted by a crash). Call it inside Exclusive.
func (s *Store) List() ([]ChunkInfo, error) {
	var out []ChunkInfo
	err := filepath.WalkDir(s.ls.Base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".tmp-") {
			_ = os.Remove(path)
			return nil
		}
		if filepath.Dir(path) == s.ls.Base { // the marker file
			return nil
		}
		id, err := desync.ChunkIDFromString(strings.TrimSuffix(name, ".cacnk"))
		if err != nil {
			return nil // not a chunk
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, ChunkInfo{ID: id, Path: path, Size: info.Size()})
		return nil
	})
	return out, err
}

// Remove deletes one chunk file and drops it from the read cache.
func (s *Store) Remove(c ChunkInfo) error {
	s.cache.remove(c.ID)
	return os.Remove(c.Path)
}

// Dir is the store directory.
func (s *Store) Dir() string { return s.ls.Base }

func (l *lru) remove(id desync.ChunkID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.items[id]; ok {
		l.order.Remove(e)
		delete(l.items, id)
		l.size -= int64(len(e.Value.(*entry).data))
	}
}

// marker records what a store holds, so a store made with another chunk ID
// algorithm is never mounted (its chunks would all look missing).
type marker struct {
	Format  int    `json:"format"`
	Digest  string `json:"digest"`
	Created string `json:"created"`
}

const markerFile = "kv-chunk-fs.json"

// CheckMarker verifies the store's marker, writing it for a new store.
func CheckMarker(dir, digest string) error {
	path := filepath.Join(dir, markerFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		entries, _ := os.ReadDir(dir)
		if len(entries) > 0 {
			return fmt.Errorf("store %s has chunks but no %s: made by another tool or version", dir, markerFile)
		}
		b, _ = json.MarshalIndent(marker{1, digest, time.Now().UTC().Format(time.RFC3339)}, "", "  ")
		return os.WriteFile(path, append(b, '\n'), 0o600)
	}
	if err != nil {
		return err
	}
	var m marker
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if m.Format != 1 || m.Digest != digest {
		return fmt.Errorf("store %s: format %d, digest %q; this build uses format 1, digest %q", dir, m.Format, m.Digest, digest)
	}
	return nil
}
