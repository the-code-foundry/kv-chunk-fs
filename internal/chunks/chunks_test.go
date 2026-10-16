package chunks

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// testBlob writes ~6 MiB of pseudo-random data with a repeated region (so some
// chunks are shared), imports it, and returns the original bytes and the File.
func testBlob(t *testing.T, cacheBytes int64) ([]byte, *File, *Store) {
	t.Helper()
	dir := t.TempDir()
	rng := rand.New(rand.NewPCG(1, 2))
	part := make([]byte, 2<<20)
	for i := range part {
		part[i] = byte(rng.Uint32())
	}
	data := append(append(append([]byte{}, part...), part...), part[:len(part)/2]...)
	tail := make([]byte, 1<<20+12345)
	for i := range tail {
		tail[i] = byte(rng.Uint32())
	}
	data = append(data, tail...)

	src := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(dir, "store")
	if err := os.Mkdir(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, "blob.caibx")
	if _, err := Import(context.Background(), src, storeDir, indexPath, DefaultChunkSizes, false, 4); err != nil {
		t.Fatal(err)
	}
	st, err := Open(storeDir, Options{Verify: true, CacheBytes: cacheBytes, Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	f, err := OpenIndex(st, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	return data, f, st
}

func TestFullRead(t *testing.T) {
	data, f, _ := testBlob(t, 64<<20)
	if f.Size() != int64(len(data)) {
		t.Fatalf("size %d, want %d", f.Size(), len(data))
	}
	got := make([]byte, len(data))
	n, err := f.ReadAt(got, 0)
	if err != nil || n != len(data) {
		t.Fatalf("ReadAt: %d, %v", n, err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
}

func TestRandomReads(t *testing.T) {
	data, f, _ := testBlob(t, 1<<20) // small cache: forces evictions
	rng := rand.New(rand.NewPCG(3, 4))
	for range 500 {
		off := rng.Int64N(int64(len(data)))
		size := 1 + rng.IntN(1<<20)
		got := make([]byte, size)
		n, err := f.ReadAt(got, off)
		want := data[off:min(off+int64(size), int64(len(data)))]
		if n != len(want) || !bytes.Equal(got[:n], want) {
			t.Fatalf("ReadAt(%d, %d): n=%d err=%v, content mismatch", off, size, n, err)
		}
		if n < size && err != io.EOF {
			t.Fatalf("short read at end without EOF: %v", err)
		}
	}
}

func TestSequentialReaderPrefetches(t *testing.T) {
	data, f, st := testBlob(t, 64<<20)
	r := f.NewReader(2 << 20)
	var out bytes.Buffer
	buf := make([]byte, 128<<10)
	for off := int64(0); off < f.Size(); {
		n, err := r.ReadAt(buf, off)
		out.Write(buf[:n])
		off += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("sequential content differs")
	}
	if st.Stats().Prefetched == 0 {
		t.Fatal("no read-ahead was scheduled")
	}
}

func TestPastEnd(t *testing.T) {
	_, f, _ := testBlob(t, 64<<20)
	n, err := f.ReadAt(make([]byte, 10), f.Size())
	if n != 0 || err != io.EOF {
		t.Fatalf("read past end: %d, %v", n, err)
	}
}

func TestLRUBound(t *testing.T) {
	l := newLRU(100)
	var id [32]byte
	for i := range 50 {
		id[0] = byte(i)
		l.put(id, make([]byte, 10))
	}
	if l.size > 100 {
		t.Fatalf("cache holds %d bytes, bound 100", l.size)
	}
	id[0] = 49
	if !l.has(id) {
		t.Fatal("most recent entry evicted")
	}
}

func TestStreamWriterDedups(t *testing.T) {
	data, _, st := testBlob(t, 64<<20)
	write := func(b []byte) ([]byte, WriteStats) {
		w := st.NewStreamWriter(context.Background())
		for off := 0; off < len(b); off += 1 << 20 { // 1 MiB pieces, like FUSE writes
			if _, err := w.Write(b[off:min(off+1<<20, len(b))]); err != nil {
				t.Fatal(err)
			}
		}
		idx, stats, err := w.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, IndexSize(idx))
		if _, err := NewFile(st, idx.Chunks).ReadAt(got, 0); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		return got, stats
	}
	got, stats := write(data)
	if !bytes.Equal(got, data) {
		t.Fatal("streamed content differs")
	}
	if stats.NewBytes != 0 {
		t.Fatalf("data already imported, yet %d new bytes stored", stats.NewBytes)
	}
	changed := append([]byte{}, data...)
	copy(changed[3<<20:], bytes.Repeat([]byte{7}, 1000)) // 1000 bytes changed in the middle
	got, stats = write(changed)
	if !bytes.Equal(got, changed) {
		t.Fatal("streamed content differs after a change")
	}
	if stats.NewChunks == 0 || stats.NewBytes > 1<<20 {
		t.Fatalf("1000 changed bytes stored %d new chunks, %d bytes", stats.NewChunks, stats.NewBytes)
	}
}
