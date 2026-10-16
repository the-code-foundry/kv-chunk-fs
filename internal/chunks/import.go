package chunks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/folbricht/desync"
)

// ChunkSizes are the content-defined chunker's min, average and max sizes in bytes.
type ChunkSizes struct{ Min, Avg, Max uint64 }

// DefaultChunkSizes is 16:64:256 KiB, desync's default. Measured on KV snapshots,
// 4:16:64 deduplicates no better and makes four times as many chunks.
var DefaultChunkSizes = ChunkSizes{16 << 10, 64 << 10, 256 << 10}

// Import chunks the file at src into the store in storeDir and writes its index
// to indexPath (atomically: a temporary file renamed into place).
func Import(ctx context.Context, src, storeDir, indexPath string, sizes ChunkSizes, compressed bool, workers int) (desync.Index, error) {
	ls, err := desync.NewLocalStore(storeDir, desync.StoreOptions{Uncompressed: !compressed})
	if err != nil {
		return desync.Index{}, err
	}
	f, err := os.Open(src)
	if err != nil {
		return desync.Index{}, err
	}
	defer f.Close()
	chunker, err := desync.NewChunker(f, sizes.Min, sizes.Avg, sizes.Max)
	if err != nil {
		return desync.Index{}, err
	}
	idx, err := desync.ChunkStream(ctx, chunker, ls, workers)
	if err != nil {
		return desync.Index{}, fmt.Errorf("chunking %s: %w", src, err)
	}
	return idx, writeIndex(idx, indexPath)
}

func writeIndex(idx desync.Index, path string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".index-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := idx.WriteTo(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
