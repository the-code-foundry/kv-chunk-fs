package metrics

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	var r Registry
	c := r.Counter("kvcfs_saves_total", "Saves.", "model")
	c.Inc("a")
	c.Add(2, "b")
	h := r.Histogram("kvcfs_save_seconds", "Save time.", []float64{0.5, 1}, "model")
	h.Observe(0.2, "a")
	h.Observe(0.7, "a")
	h.Observe(3, "a")
	r.Gauge("kvcfs_store_bytes", "Store.", func() float64 { return 1234 })
	var b strings.Builder
	r.Render(&b)
	out := b.String()
	for _, want := range []string{
		`kvcfs_saves_total{model="a"} 1`,
		`kvcfs_saves_total{model="b"} 2`,
		`kvcfs_save_seconds_bucket{model="a",le="0.5"} 1`,
		`kvcfs_save_seconds_bucket{model="a",le="1"} 2`,
		`kvcfs_save_seconds_bucket{model="a",le="+Inf"} 3`,
		`kvcfs_save_seconds_count{model="a"} 3`,
		`kvcfs_store_bytes 1234`,
		"# TYPE kvcfs_save_seconds histogram",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
