// Package api serves kv-chunk-fs's HTTP API: status, Prometheus metrics, the
// log buffer, snapshots with statistics and labels, token-prefix trees, and
// garbage collection. It listens on TCP and/or a Unix socket, each only when
// configured; there is no authentication.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kvchunkfs/internal/events"
	"kvchunkfs/internal/kvfs"
)

// Server is the API of one mount.
type Server struct {
	fs      *kvfs.FS
	mount   string // deletes go through the mount, so they are events like any other
	servers []*http.Server
}

// New returns the API of the mount at mountpoint.
func New(k *kvfs.FS, mountpoint string) *Server { return &Server{fs: k, mount: mountpoint} }

// Listen starts the configured listeners: TCP when port is set (host defaults to
// 127.0.0.1), a Unix socket when socket is set. It returns their addresses.
func (s *Server) Listen(host, port, socket string) ([]string, error) {
	var addrs []string
	h := s.Handler()
	start := func(l net.Listener, name string) {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		s.servers = append(s.servers, srv)
		addrs = append(addrs, name)
		go func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.fs.Events().Errorf("api %s: %v", name, err)
			}
		}()
	}
	if port != "" {
		if host == "" {
			host = "127.0.0.1"
		}
		addr := net.JoinHostPort(host, port)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("api: %w", err)
		}
		start(l, "http://"+addr)
	}
	if socket != "" {
		if st, err := os.Lstat(socket); err == nil && st.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(socket) // left over from a previous run
		}
		l, err := net.Listen("unix", socket)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("api: %w", err)
		}
		_ = os.Chmod(socket, 0o660)
		start(l, "unix:"+socket)
	}
	return addrs, nil
}

// Close stops the listeners.
func (s *Server) Close() {
	for _, srv := range s.servers {
		_ = srv.Close()
	}
}

// Handler routes the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /logs", s.logs)
	mux.HandleFunc("GET /snapshots", s.snapshots)
	mux.HandleFunc("/snapshots/", s.snapshot) // ids contain slashes: routed by hand
	mux.HandleFunc("GET /tree", s.tree)
	mux.HandleFunc("POST /gc", s.gc)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": []string{
		"GET /status", "GET /metrics", "GET /logs?since=&type=&level=&limit=",
		"GET /snapshots?model=&sort=score|size|unique|uses|last_use|idle|tokens|id&order=desc|asc&limit=",
		"GET /snapshots/<id>", "DELETE /snapshots/<id>",
		"PUT /snapshots/<id>/labels/<label> (body or ?value=)", "DELETE /snapshots/<id>/labels/<label>",
		"GET /tree?model=&summary=1", "POST /gc",
	}})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.fs.Status())
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.fs.Metrics(w)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	entries, last := s.fs.Events().Find(events.Query{Since: since, Type: q.Get("type"), Level: q.Get("level"), Limit: limit})
	if entries == nil {
		entries = []events.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next": last, "buffer": s.fs.Events().Size()})
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	list, err := s.fs.Snapshots()
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	q := r.URL.Query()
	if m := q.Get("model"); m != "" {
		kept := list[:0]
		for _, sn := range list {
			if sn.Model == m {
				kept = append(kept, sn)
			}
		}
		list = kept
	}
	kvfs.SortSnapshots(list, q.Get("sort"), q.Get("order") == "asc")
	var size, unique int64
	for _, sn := range list {
		size += sn.Size
		unique += sn.UniqueBytes
	}
	total := len(list)
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n >= 0 && n < len(list) {
		list = list[:n]
	}
	if list == nil {
		list = []kvfs.Snapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "size": size, "unique_bytes": unique, "snapshots": list})
}

// snapshot handles /snapshots/<id> and /snapshots/<id>/labels/<label>.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/snapshots/")
	id, label, isLabel := rest, "", false
	if i := strings.LastIndex(rest, "/labels/"); i >= 0 {
		id, label, isLabel = rest[:i], rest[i+len("/labels/"):], true
	}
	if id == "" {
		fail(w, http.StatusNotFound, "no snapshot id")
		return
	}
	if isLabel {
		s.label(w, r, id, label)
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := s.fs.Snapshots()
		if err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		for _, sn := range list {
			if sn.ID == id {
				writeJSON(w, http.StatusOK, sn)
				return
			}
		}
		fail(w, http.StatusNotFound, "no snapshot %q", id)
	case http.MethodDelete:
		path := filepath.Join(s.mount, filepath.Clean("/" + id))
		if err := os.Remove(path); err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, os.ErrNotExist) {
				code = http.StatusNotFound
			}
			fail(w, code, "%v", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, DELETE")
		fail(w, http.StatusMethodNotAllowed, "use GET or DELETE")
	}
}

func (s *Server) label(w http.ResponseWriter, r *http.Request, id, label string) {
	if !kvfs.ValidLabel(label) {
		fail(w, http.StatusBadRequest, "label %q: use 1-64 of A-Z a-z 0-9 . _ -", label)
		return
	}
	var value *string
	switch r.Method {
	case http.MethodPut:
		v := r.URL.Query().Get("value")
		if !r.URL.Query().Has("value") {
			b, err := io.ReadAll(io.LimitReader(r.Body, 4096))
			if err != nil {
				fail(w, http.StatusBadRequest, "%v", err)
				return
			}
			v = strings.TrimRight(string(b), "\r\n")
		}
		value = &v
	case http.MethodDelete:
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		fail(w, http.StatusMethodNotAllowed, "use PUT or DELETE")
		return
	}
	if err := s.fs.SetLabel(id, label, value); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, syscall.ENOENT) {
			code = http.StatusNotFound
		} else if errors.Is(err, syscall.EINVAL) {
			code = http.StatusBadRequest
		}
		fail(w, code, "%s: %v", id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request) {
	trees, err := s.fs.Trees(r.URL.Query().Get("model"))
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if r.URL.Query().Get("summary") != "" {
		for i := range trees {
			trees[i].Tree = nil
		}
	}
	if trees == nil {
		trees = []kvfs.ModelTree{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": trees})
}

func (s *Server) gc(w http.ResponseWriter, r *http.Request) {
	res := s.fs.Collect()
	out := map[string]any{
		"seconds": res.Took.Seconds(), "snapshots": res.Stubs, "chunks": res.Chunks,
		"store_bytes": res.StoreBytes, "logical_bytes": res.LogicalBytes,
		"removed_chunks": res.Removed, "removed_bytes": res.RemovedBytes, "broken_removed": res.Broken,
	}
	if res.Err != nil {
		out["error"] = res.Err.Error()
		writeJSON(w, http.StatusConflict, out)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
