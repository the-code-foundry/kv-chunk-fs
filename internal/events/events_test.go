package events

import (
	"bytes"
	"strings"
	"testing"
)

func TestRingKeepsNewestAndCursor(t *testing.T) {
	var out bytes.Buffer
	l := New(3, &out)
	for i := range 5 {
		l.Emit("info", "save", "saved", map[string]any{"n": i})
	}
	got, last := l.Find(Query{})
	if len(got) != 3 || got[0].ID != 3 || got[2].ID != 5 || last != 5 {
		t.Fatalf("got %d entries, first %d, last %d", len(got), got[0].ID, last)
	}
	got, _ = l.Find(Query{Since: 4})
	if len(got) != 1 || got[0].ID != 5 {
		t.Fatalf("since 4: %+v", got)
	}
	if !strings.Contains(out.String(), "event=save saved n=4") {
		t.Fatalf("journal line: %q", out.String())
	}
}

func TestFilters(t *testing.T) {
	l := New(10, &bytes.Buffer{})
	l.Emit("info", "save", "a", nil)
	l.Emit("error", "log", "b", nil)
	l.Emit("info", "delete", "c", nil)
	if got, _ := l.Find(Query{Type: "save,delete"}); len(got) != 2 {
		t.Fatalf("type filter: %d", len(got))
	}
	if got, _ := l.Find(Query{Level: "error"}); len(got) != 1 || got[0].Msg != "b" {
		t.Fatalf("level filter: %+v", got)
	}
	if got, _ := l.Find(Query{Limit: 1}); len(got) != 1 || got[0].Msg != "c" {
		t.Fatalf("limit keeps newest: %+v", got)
	}
}

func TestZeroSizeStillLogs(t *testing.T) {
	var out bytes.Buffer
	l := New(0, &out)
	l.Infof("hello %d", 1)
	if got, last := l.Find(Query{}); len(got) != 0 || last != 1 || !strings.Contains(out.String(), "hello 1") {
		t.Fatalf("got %v last %d out %q", got, last, out.String())
	}
}
