// Package events is the log of kv-chunk-fs: every line goes to the journal
// (stderr) and into an in-memory ring buffer that the API serves with a cursor.
// Structured events (save, restore, delete, gc, ...) carry fields; plain log
// lines are type "log".
package events

import (
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one log line or event.
type Entry struct {
	ID     uint64         `json:"id"`
	Time   time.Time      `json:"time"`
	Level  string         `json:"level"` // info, error
	Type   string         `json:"type"`  // log, save, restore, delete, gc, broken, ...
	Msg    string         `json:"msg"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Log keeps the last N entries.
type Log struct {
	mu   sync.Mutex
	buf  []Entry
	head int // next slot to write
	n    int // entries held
	last uint64
	out  *log.Logger
}

// New returns a log keeping size entries (0 keeps none) and writing to out.
func New(size int, out io.Writer) *Log {
	if size < 0 {
		size = 0
	}
	return &Log{buf: make([]Entry, size), out: log.New(out, "", log.LstdFlags|log.Lmicroseconds)}
}

// Emit records an event and writes it as one line: msg, then key=value fields.
func (l *Log) Emit(level, typ, msg string, fields map[string]any) {
	line := msg
	if len(fields) > 0 {
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString(msg)
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%v", k, fields[k])
		}
		line = b.String()
	}
	if typ != "log" {
		line = "event=" + typ + " " + line
	}
	if level == "error" {
		line = "ERROR " + line
	}
	l.out.Print(line)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.last++
	if len(l.buf) == 0 {
		return
	}
	l.buf[l.head] = Entry{ID: l.last, Time: time.Now(), Level: level, Type: typ, Msg: msg, Fields: fields}
	l.head = (l.head + 1) % len(l.buf)
	if l.n < len(l.buf) {
		l.n++
	}
}

// Infof logs a plain line.
func (l *Log) Infof(format string, args ...any) {
	l.Emit("info", "log", fmt.Sprintf(format, args...), nil)
}

// Errorf logs a plain error line.
func (l *Log) Errorf(format string, args ...any) {
	l.Emit("error", "log", fmt.Sprintf(format, args...), nil)
}

// Writer adapts the log for the standard library's log package (go-fuse's
// debug output, for instance): each written line becomes a "log" entry.
func (l *Log) Writer() io.Writer { return writerFunc(func(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			l.Emit("info", "log", line, nil)
		}
	}
	return len(p), nil
}) }

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Query selects entries for the API.
type Query struct {
	Since uint64 // only entries with a larger id
	Type  string // "" for all; comma-separated list
	Level string // "" for all
	Limit int    // 0: no limit; otherwise the newest Limit matches
}

// Find returns the matching entries, oldest first, and the newest id so far
// (the cursor for the next call).
func (l *Log) Find(q Query) ([]Entry, uint64) {
	types := map[string]bool{}
	for _, t := range strings.Split(q.Type, ",") {
		if t = strings.TrimSpace(t); t != "" {
			types[t] = true
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Entry
	start := (l.head - l.n + len(l.buf)) % max(len(l.buf), 1)
	for i := 0; i < l.n; i++ {
		e := l.buf[(start+i)%len(l.buf)]
		if e.ID <= q.Since || (len(types) > 0 && !types[e.Type]) || (q.Level != "" && e.Level != q.Level) {
			continue
		}
		out = append(out, e)
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, l.last
}

// Size is the buffer's capacity.
func (l *Log) Size() int { return len(l.buf) }
