package chunks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/folbricht/desync"
)

// Throughput of the pieces of the write path, on 256 MiB of random data:
//
//	go test -run XXX -bench . -benchtime 3x ./internal/chunks/
//
// (generated on first use, so plain `go test` does not pay for it)
var benchBytes = sync.OnceValue(func() []byte {
	b := make([]byte, 256<<20)
	r := rand.New(rand.NewPCG(5, 6))
	for i := 0; i < len(b); i += 8 {
		v := r.Uint64()
		for j := 0; j < 8; j++ {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	return b
})

func BenchmarkChunkerOnly(b *testing.B) {
	b.SetBytes(int64(len(benchBytes())))
	for range b.N {
		c, _ := desync.NewChunker(bytes.NewReader(benchBytes()), DefaultChunkSizes.Min, DefaultChunkSizes.Avg, DefaultChunkSizes.Max)
		for {
			_, buf, err := c.Next()
			if err != nil || len(buf) == 0 {
				break
			}
		}
	}
}

func BenchmarkSHA512_256(b *testing.B) {
	b.SetBytes(int64(len(benchBytes())))
	for range b.N {
		sha512.Sum512_256(benchBytes())
	}
}

func BenchmarkSHA256(b *testing.B) {
	b.SetBytes(int64(len(benchBytes())))
	for range b.N {
		sha256.Sum256(benchBytes())
	}
}

// the whole streaming path into a store that already has every chunk
func BenchmarkStreamWriterAllKnown(b *testing.B) {
	st, err := Open(b.TempDir(), Options{CacheBytes: 1 << 20, Workers: 8})
	if err != nil {
		b.Fatal(err)
	}
	if _, _, err := st.ChunkReader(context.Background(), bytes.NewReader(benchBytes())); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(benchBytes())))
	b.ResetTimer()
	for range b.N {
		w := st.NewStreamWriter(context.Background())
		for off := 0; off < len(benchBytes()); off += 1 << 20 {
			_, _ = w.Write(benchBytes()[off : off+1<<20])
		}
		_, _, _ = w.Close()
	}
}


func BenchmarkStreamWriterAllKnownSHA256(b *testing.B) {
	old := desync.Digest
	desync.Digest = desync.SHA256{}
	defer func() { desync.Digest = old }()
	BenchmarkStreamWriterAllKnown(b)
}

func BenchmarkChunkReaderBytes(b *testing.B) {
	st, _ := Open(b.TempDir(), Options{CacheBytes: 1 << 20, Workers: 8})
	_, _, _ = st.ChunkReader(context.Background(), bytes.NewReader(benchBytes()))
	b.SetBytes(int64(len(benchBytes())))
	b.ResetTimer()
	for range b.N {
		_, _, _ = st.ChunkReader(context.Background(), bytes.NewReader(benchBytes()))
	}
}
