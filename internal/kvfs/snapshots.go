package kvfs

import (
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/folbricht/desync"

	"kvchunkfs/internal/slotfile"
)

// ScoreWeights configure the eviction score of a snapshot. kv-chunk-fs never
// evicts; the score is a suggestion for whatever does (higher = evict sooner):
//
//	score = (1 + Unique*unique_GiB + Size*size_GiB) * 2^(idle/HalfLife) / (1 + Uses*uses)
//
// idle is the time since the last use (or since it was written, if never read).
type ScoreWeights struct {
	Unique   float64
	Size     float64
	Uses     float64
	HalfLife time.Duration
}

// Snapshot describes one chunked file.
type Snapshot struct {
	ID          string    `json:"id"` // path relative to the mount
	Model       string    `json:"model"`
	Size        int64     `json:"size"`
	Chunks      int       `json:"chunks"`
	UniqueBytes int64     `json:"unique_bytes"` // freed if it is deleted
	SharedBytes int64     `json:"shared_bytes"`
	Modified    time.Time `json:"modified"`
	Meta
	IdleSeconds int64   `json:"idle_seconds"`
	Score       float64 `json:"score"`
	Tokens      int     `json:"tokens,omitempty"`
	Format      string  `json:"format"` // "server-tokens v1", "plain", or "unparsed: <reason>"

	tokens []int32
}

// parsed caches what a stub's index and header say, per stub version.
type parsed struct {
	ino     uint64
	mtimeNs int64
	size    int64
	chunks  []desync.IndexChunk
	header  *slotfile.Header
	err     string
}

func (k *FS) parse(path string, size int64) *parsed {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return &parsed{err: err.Error()}
	}
	mt := st.Mtim.Sec*1e9 + st.Mtim.Nsec
	k.mu.Lock()
	p, ok := k.parsedCache[path]
	k.mu.Unlock()
	if ok && p.ino == st.Ino && p.mtimeNs == mt && p.size == size {
		return p
	}
	p = &parsed{ino: st.Ino, mtimeNs: mt, size: size}
	if size > 0 {
		blob, err := k.openBlob(path, size)
		if err != nil {
			p.err = "unreadable index: " + err.Error()
		} else {
			p.chunks = blob.Chunks()
			if h, err := slotfile.Parse(blob, size); err != nil {
				p.err = err.Error()
			} else {
				p.header = h
			}
		}
	} else {
		p.err = "empty file"
	}
	k.mu.Lock()
	if k.parsedCache == nil {
		k.parsedCache = map[string]*parsed{}
	}
	k.parsedCache[path] = p
	k.mu.Unlock()
	return p
}

// Snapshots lists every chunked file with its statistics, metadata and score.
func (k *FS) Snapshots() ([]Snapshot, error) {
	type entry struct {
		snap Snapshot
		p    *parsed
	}
	var list []entry
	seen := map[string]bool{}
	err := filepath.WalkDir(k.tree, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		size, ok := stubSize(path)
		if !ok {
			return nil
		}
		seen[path] = true
		rel := k.rel(path)
		s := Snapshot{ID: rel, Model: modelOf(rel), Size: size, Meta: readMeta(path)}
		if info, err := os.Stat(path); err == nil {
			s.Modified = info.ModTime()
		}
		p := k.parse(path, size)
		s.Chunks = len(p.chunks)
		if p.header != nil {
			s.Format, s.Tokens, s.tokens = p.header.Envelope, len(p.header.Tokens), p.header.Tokens
		} else {
			s.Format = "unparsed: " + p.err
		}
		list = append(list, entry{s, p})
		return nil
	})
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	for path := range k.parsedCache { // forget deleted stubs
		if !seen[path] {
			delete(k.parsedCache, path)
		}
	}
	w := k.opt.Score
	k.mu.Unlock()

	// how many snapshots use each chunk, counting a chunk once per snapshot
	refs := map[desync.ChunkID]int{}
	for _, e := range list {
		uniq := map[desync.ChunkID]bool{}
		for _, c := range e.p.chunks {
			if !uniq[c.ID] {
				uniq[c.ID] = true
				refs[c.ID]++
			}
		}
	}
	out := make([]Snapshot, len(list))
	now := time.Now()
	for i, e := range list {
		s := e.snap
		uniq := map[desync.ChunkID]bool{}
		for _, c := range e.p.chunks {
			if uniq[c.ID] {
				continue
			}
			uniq[c.ID] = true
			if refs[c.ID] == 1 {
				s.UniqueBytes += int64(c.Size)
			} else {
				s.SharedBytes += int64(c.Size)
			}
		}
		last := s.LastUse
		if last.IsZero() {
			last = s.Modified
		}
		s.IdleSeconds = int64(now.Sub(last).Seconds())
		s.Score = score(s, w)
		out[i] = s
	}
	return out, nil
}

func score(s Snapshot, w ScoreWeights) float64 {
	const gib = 1 << 30
	half := w.HalfLife.Seconds()
	if half <= 0 {
		half = 86400
	}
	cost := 1 + w.Unique*float64(s.UniqueBytes)/gib + w.Size*float64(s.Size)/gib
	v := cost * math.Pow(2, float64(s.IdleSeconds)/half) / (1 + w.Uses*float64(s.Uses))
	return math.Round(v*1000) / 1000
}

// SortSnapshots orders snapshots by a field: score (default), size, unique,
// uses, last_use, idle, tokens, id; descending unless asc.
func SortSnapshots(list []Snapshot, by string, asc bool) {
	key := func(s Snapshot) float64 {
		switch by {
		case "size":
			return float64(s.Size)
		case "unique":
			return float64(s.UniqueBytes)
		case "uses":
			return float64(s.Uses)
		case "last_use":
			return float64(s.LastUse.Unix())
		case "idle":
			return float64(s.IdleSeconds)
		case "tokens":
			return float64(s.Tokens)
		}
		return s.Score
	}
	sort.SliceStable(list, func(i, j int) bool {
		if by == "id" {
			return (list[i].ID < list[j].ID) == asc
		}
		if asc {
			return key(list[i]) < key(list[j])
		}
		return key(list[i]) > key(list[j])
	})
}

// ModelTree is the token-prefix tree of one model's snapshots.
type ModelTree struct {
	Model    string            `json:"model"`
	Summary  []slotfile.Shared `json:"summary"` // shared prefixes, largest groups first
	Tree     *slotfile.Node    `json:"tree"`
	Unparsed map[string]string `json:"unparsed,omitempty"` // id -> reason
}

// Trees builds the token-prefix tree per model (tokenizers differ between
// models, so only a model's own snapshots are compared).
func (k *FS) Trees(model string) ([]ModelTree, error) {
	snaps, err := k.Snapshots()
	if err != nil {
		return nil, err
	}
	byModel := map[string]*ModelTree{}
	items := map[string][]slotfile.Item{}
	for _, s := range snaps {
		if model != "" && s.Model != model {
			continue
		}
		t, ok := byModel[s.Model]
		if !ok {
			t = &ModelTree{Model: s.Model}
			byModel[s.Model] = t
		}
		if s.tokens == nil {
			if t.Unparsed == nil {
				t.Unparsed = map[string]string{}
			}
			t.Unparsed[s.ID] = s.Format
			continue
		}
		items[s.Model] = append(items[s.Model], slotfile.Item{ID: s.ID, Tokens: s.tokens})
	}
	var out []ModelTree
	for m, t := range byModel {
		t.Tree = slotfile.Build(items[m])
		t.Summary = slotfile.Summary(t.Tree)
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

