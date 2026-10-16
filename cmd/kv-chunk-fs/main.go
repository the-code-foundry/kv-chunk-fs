// kv-chunk-fs: a deduplicating FUSE filesystem for llama.cpp KV-cache snapshots.
//
//	kv-chunk-fs mount  [settings] [MNT]          mount BACKING/tree on MNT, chunks in BACKING/store
//	kv-chunk-fs config [settings]                print the effective settings and their sources
//	kv-chunk-fs import --store DIR FILE INDEX    chunk FILE into a store, write INDEX (.caibx)
//
// Settings come from defaults, a config file (--config / KVCFS_CONFIG), the
// environment (KVCFS_<NAME>) and flags (--<name>), in that order. See README.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/folbricht/desync"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"kvchunkfs/internal/api"
	"kvchunkfs/internal/chunks"
	"kvchunkfs/internal/config"
	"kvchunkfs/internal/events"
	"kvchunkfs/internal/kvfs"
)

const usage = `usage:
  kv-chunk-fs mount  [settings] [MNT]          mount BACKING/tree on MNT (read-write)
  kv-chunk-fs config [settings]                print the effective settings and where each comes from
  kv-chunk-fs import [flags] FILE INDEX        chunk FILE into a store and write its index
Settings: --config FILE, KVCFS_<NAME> environment variables, --<name> flags; see mount -h.
`

// version is set at build time (build.sh: -ldflags -X main.version=...).
var version = "dev"

// digestName is the chunk ID algorithm set in main, as the store marker records it.
const digestName = "sha256"

// settings of the mount command. Names are the KVCFS_* variables without prefix.
var settings = []config.Setting{
	{Name: "BACKING", Help: "backing directory: tree/, store/, staging/ (required)"},
	{Name: "MOUNT", Help: "mount point (or the positional argument)"},
	{Name: "CHUNK_GLOB", Default: "*.bin,*.bin.tmp", Help: "comma-separated base-name patterns stored as chunks"},
	{Name: "CHUNK_SIZE", Default: "16:64:256", Help: "min:avg:max chunk size in KiB"},
	{Name: "VERIFY", Default: "false", Help: "check each chunk's hash when it is read from disk"},
	{Name: "CACHE_MIB", Default: "256", Help: "decoded-chunk cache in MiB"},
	{Name: "READAHEAD_MIB", Default: "32", Help: "MiB prefetched past a sequential reader"},
	{Name: "WORKERS", Default: "8", Help: "goroutines prefetching and storing chunks"},
	{Name: "ALLOW_OTHER", Default: "true", Help: "let other users (root: the Docker daemon binding the mount) in; the kernel still enforces file permissions (needs user_allow_other in /etc/fuse.conf)"},
	{Name: "GC_INTERVAL", Default: "1h", Help: "garbage collection interval (0: only at start, on SIGUSR2 and POST /gc)"},
	{Name: "GC_WAIT", Default: "1m", Help: "how long a garbage collection waits for busy writers before skipping"},
	{Name: "API_HOST", Default: "", Help: "API TCP host (with API_PORT; empty: 127.0.0.1)"},
	{Name: "API_PORT", Default: "", Help: "API TCP port (empty: no TCP listener)"},
	{Name: "API_SOCKET", Default: "", Help: "API Unix socket path (empty: no socket)"},
	{Name: "LOG_BUFFER_LINES", Default: "1000", Help: "log lines and events kept in memory for GET /logs"},
	{Name: "USE_WINDOW", Default: "30s", Help: "reads of a snapshot within this window count as one use (a restore opens it several times)"},
	{Name: "SCORE_UNIQUE_WEIGHT", Default: "1", Help: "eviction score: weight of unique GiB"},
	{Name: "SCORE_SIZE_WEIGHT", Default: "0", Help: "eviction score: weight of logical GiB"},
	{Name: "SCORE_USES_WEIGHT", Default: "1", Help: "eviction score: weight of uses (restores)"},
	{Name: "SCORE_HALF_LIFE", Default: "24h", Help: "eviction score: idle time that doubles the score"},
	{Name: "DEBUG", Default: "false", Help: "log every FUSE request"},
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	// chunk IDs: SHA-256 runs on the CPU's SHA instructions (1.7 GB/s here,
	// against 0.5 GB/s for desync's default SHA-512/256). A store holds one
	// kind; indexes record which.
	desync.Digest = desync.SHA256{} // digestName records it in the store
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "import":
		err = runImport(os.Args[2:])
	case "mount":
		err = runMount(os.Args[2:])
	case "config":
		err = runConfig(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// loadConfig parses the settings' flags and resolves them.
func loadConfig(name string, args []string) (*config.Config, *flag.FlagSet, error) {
	fl := flag.NewFlagSet(name, flag.ExitOnError)
	all := config.Register(fl, settings)
	_ = fl.Parse(args)
	c, err := config.Resolve(fl, all, os.Environ())
	return c, fl, err
}

func runConfig(args []string) error {
	c, _, err := loadConfig("config", args)
	if err != nil {
		return err
	}
	fmt.Print(c.Dump())
	return nil
}

// mountOptions are the typed settings.
type mountOptions struct {
	backing, mount                  string
	globs                           []string
	sizes                           chunks.ChunkSizes
	verify, allowOther, debug       bool
	cacheMiB, readaheadMiB, workers int64
	gcEvery, gcWait, useWindow      time.Duration
	apiHost, apiPort, apiSocket     string
	logLines                        int64
	score                           kvfs.ScoreWeights
}

func typed(c *config.Config, fl *flag.FlagSet) (mountOptions, error) {
	o := mountOptions{backing: c.String("BACKING"), mount: c.String("MOUNT"), globs: c.List("CHUNK_GLOB"),
		apiHost: c.String("API_HOST"), apiPort: c.String("API_PORT"), apiSocket: c.String("API_SOCKET")}
	if fl.NArg() == 1 {
		o.mount = fl.Arg(0)
	} else if fl.NArg() > 1 {
		return o, fmt.Errorf("one mount point, got %v", fl.Args())
	}
	if o.backing == "" || o.mount == "" {
		return o, fmt.Errorf("BACKING and MOUNT are required (--backing DIR MNT, KVCFS_BACKING/KVCFS_MOUNT, or the config file)")
	}
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	var err error
	o.sizes, err = parseSizes(c.String("CHUNK_SIZE"))
	collect(err)
	o.verify, err = c.Bool("VERIFY")
	collect(err)
	o.allowOther, err = c.Bool("ALLOW_OTHER")
	collect(err)
	o.debug, err = c.Bool("DEBUG")
	collect(err)
	o.cacheMiB, err = c.Int("CACHE_MIB")
	collect(err)
	o.readaheadMiB, err = c.Int("READAHEAD_MIB")
	collect(err)
	o.workers, err = c.Int("WORKERS")
	collect(err)
	o.logLines, err = c.Int("LOG_BUFFER_LINES")
	collect(err)
	o.gcEvery, err = c.Duration("GC_INTERVAL")
	collect(err)
	o.gcWait, err = c.Duration("GC_WAIT")
	collect(err)
	o.useWindow, err = c.Duration("USE_WINDOW")
	collect(err)
	o.score.Unique, err = c.Float("SCORE_UNIQUE_WEIGHT")
	collect(err)
	o.score.Size, err = c.Float("SCORE_SIZE_WEIGHT")
	collect(err)
	o.score.Uses, err = c.Float("SCORE_USES_WEIGHT")
	collect(err)
	o.score.HalfLife, err = c.Duration("SCORE_HALF_LIFE")
	collect(err)
	if o.apiPort != "" {
		if _, err := strconv.Atoi(o.apiPort); err != nil {
			collect(fmt.Errorf("API_PORT=%q: not a port", o.apiPort))
		}
	}
	if len(errs) > 0 {
		return o, fmt.Errorf("configuration: %v", errs)
	}
	return o, nil
}

func runImport(args []string) error {
	fl := flag.NewFlagSet("import", flag.ExitOnError)
	store := fl.String("store", "", "chunk store directory (required)")
	sizes := fl.String("chunk-size", "16:64:256", "min:avg:max chunk size in KiB")
	compressed := fl.Bool("compressed", false, "zstd-compress chunks (KV data gains ~4%, so off)")
	workers := fl.Int("workers", 8, "goroutines storing chunks")
	_ = fl.Parse(args)
	if *store == "" || fl.NArg() != 2 {
		return fmt.Errorf("usage: kv-chunk-fs import --store DIR FILE INDEX")
	}
	cs, err := parseSizes(*sizes)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*store, 0o700); err != nil {
		return err
	}
	start := time.Now()
	idx, err := chunks.Import(context.Background(), fl.Arg(0), *store, fl.Arg(1), cs, *compressed, *workers)
	if err != nil {
		return err
	}
	log.Printf("imported %s: %d bytes, %d chunks in %s", fl.Arg(0), chunks.IndexSize(idx), len(idx.Chunks), time.Since(start).Round(time.Millisecond))
	return nil
}

func runMount(args []string) error {
	c, fl, err := loadConfig("mount", args)
	if err != nil {
		return err
	}
	o, err := typed(c, fl)
	if err != nil {
		return err
	}
	ev := events.New(int(o.logLines), os.Stderr)
	log.SetFlags(0)
	log.SetOutput(ev.Writer()) // go-fuse and library output join the log buffer
	for _, v := range c.Values() {
		if v.Source != "default" {
			ev.Infof("setting %s=%s (%s)", v.Name, v.Value, v.Source)
		}
	}

	storeDir := filepath.Join(o.backing, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return err
	}
	if err := chunks.CheckMarker(storeDir, digestName); err != nil {
		return err
	}
	st, err := chunks.Open(storeDir, chunks.Options{
		Verify: o.verify, CacheBytes: o.cacheMiB << 20, Workers: int(o.workers), Sizes: o.sizes,
	})
	if err != nil {
		return err
	}
	root, err := kvfs.NewRoot(st, kvfs.Options{
		Backing: o.backing, ChunkGlobs: o.globs, Readahead: o.readaheadMiB << 20,
		Mountpoint: o.mount, Version: version, Events: ev, Score: o.score, UseWindow: o.useWindow,
	})
	if err != nil {
		return err
	}
	// crash recovery before anything is served: interrupted chunk writes and
	// staging copies go, stubs whose chunks are missing go, orphans go
	kfs := root.FS()
	kfs.GC(0)

	ttl := time.Second
	server, err := fs.Mount(o.mount, root, &fs.Options{
		EntryTimeout: &ttl,
		AttrTimeout:  &ttl,
		MountOptions: fuse.MountOptions{
			FsName:       "kv-chunk-fs",
			Name:         "kv-chunk-fs",
			MaxWrite:     1 << 20, // also the largest read request: 1 MiB
			MaxReadAhead: 1 << 20, // the kernel caps this (128 KiB on Linux)
			AllowOther:   o.allowOther,
			// the kernel checks mode bits itself, so allow_other opens nothing
			// that the backing files' permissions do not
			Options: []string{"default_permissions"},
			// pass O_TRUNC with OPEN instead of a separate truncate afterwards,
			// so a rewrite starts a fresh stream right away
			ExtraCapabilities: fuse.CAP_ATOMIC_O_TRUNC,
			Debug:             o.debug,
		},
	})
	if err != nil {
		return fmt.Errorf("mount %s: %w", o.mount, err)
	}

	apiSrv := api.New(kfs, o.mount)
	addrs, err := apiSrv.Listen(o.apiHost, o.apiPort, o.apiSocket)
	if err != nil {
		_ = server.Unmount()
		return err
	}
	kfs.SetAPIAddrs(addrs)
	apiDesc := "off"
	if len(addrs) > 0 {
		apiDesc = strings.Join(addrs, ", ")
	}
	ev.Infof("kv-chunk-fs %s: mounted %s on %s (chunked: %s; api: %s)", version,
		filepath.Join(o.backing, "tree"), o.mount, strings.Join(o.globs, ","), apiDesc)
	sdNotify("READY=1")

	gcNow := make(chan struct{}, 1)
	go func() {
		var tick <-chan time.Time
		if o.gcEvery > 0 {
			t := time.NewTicker(o.gcEvery)
			defer t.Stop()
			tick = t.C
		}
		for {
			select {
			case <-tick:
			case <-gcNow:
			}
			kfs.GC(o.gcWait)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for s := range sig {
			switch s {
			case syscall.SIGUSR1:
				logStats(ev, st)
			case syscall.SIGUSR2:
				select {
				case gcNow <- struct{}{}:
				default:
				}
			default:
				sdNotify("STOPPING=1")
				logStats(ev, st)
				apiSrv.Close()
				if err := server.Unmount(); err != nil {
					ev.Errorf("unmount: %v (busy? retry, or fusermount3 -uz %s)", err, o.mount)
				}
			}
		}
	}()
	server.Wait()
	apiSrv.Close()
	if o.apiSocket != "" {
		_ = os.Remove(o.apiSocket)
	}
	logStats(ev, st)
	return nil
}

// sdNotify tells systemd (Type=notify) about state changes; a no-op elsewhere.
func sdNotify(state string) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	// an abstract socket ("@...") is handled by the net package itself
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		log.Printf("sd_notify: %v", err)
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(state))
}

func logStats(ev *events.Log, st *chunks.Store) {
	s := st.Stats()
	ev.Infof("stats: %d cache hits, %d misses, %d chunks read from disk (%d MiB), %d prefetches queued, %d dropped",
		s.Hits, s.Misses, s.Fetched, s.FetchedBytes>>20, s.Prefetched, s.PrefetchDropped)
}

func parseSizes(s string) (chunks.ChunkSizes, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return chunks.ChunkSizes{}, fmt.Errorf("chunk size %q: want min:avg:max in KiB", s)
	}
	var v [3]uint64
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return chunks.ChunkSizes{}, fmt.Errorf("chunk size %q: %w", s, err)
		}
		v[i] = n << 10
	}
	return chunks.ChunkSizes{Min: v[0], Avg: v[1], Max: v[2]}, nil
}
