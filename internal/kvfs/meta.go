package kvfs

import (
	"context"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
)

// Snapshot metadata lives in extended attributes on the stub, next to the size:
// it moves with rename, needs no database, and any tool can read it with
// getfattr. Times are Unix seconds.
const (
	metaPrefix   = "user.kvchunkfs."
	usesXattr    = metaPrefix + "uses"     // reads of the snapshot (restores)
	lastUseXattr = metaPrefix + "last_use" // last read
	createdXattr = metaPrefix + "created"  // first written under this name
	labelPrefix  = metaPrefix + "label."   // label.<name> = value
)

// Meta is a snapshot's metadata.
type Meta struct {
	Uses    int64             `json:"uses"`
	LastUse time.Time         `json:"last_use,omitzero"`
	Created time.Time         `json:"created,omitzero"`
	Labels  map[string]string `json:"labels,omitempty"`
}

var labelName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidLabel reports whether name can be a label.
func ValidLabel(name string) bool { return labelName.MatchString(name) }

func getx(path, name string) (string, bool) {
	buf := make([]byte, 4096)
	n, err := syscall.Getxattr(path, name, buf)
	if err != nil {
		return "", false
	}
	return string(buf[:n]), true
}

func listx(path string) []string {
	buf := make([]byte, 16384)
	n, err := syscall.Listxattr(path, buf)
	if err != nil || n <= 0 {
		return nil
	}
	var out []string
	for _, name := range strings.Split(string(buf[:n]), "\x00") {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func unixTime(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// readMeta reads the metadata on the stub at path.
func readMeta(path string) Meta {
	var m Meta
	if v, ok := getx(path, usesXattr); ok {
		m.Uses, _ = strconv.ParseInt(v, 10, 64)
	}
	if v, ok := getx(path, lastUseXattr); ok {
		m.LastUse = unixTime(v)
	}
	if v, ok := getx(path, createdXattr); ok {
		m.Created = unixTime(v)
	}
	for _, name := range listx(path) {
		if l, ok := strings.CutPrefix(name, labelPrefix); ok {
			if v, ok := getx(path, name); ok {
				if m.Labels == nil {
					m.Labels = map[string]string{}
				}
				m.Labels[l] = v
			}
		}
	}
	return m
}

// copyMeta carries the metadata of the stub at from onto the stub at to, which
// replaces it (a rewrite, or the proxy renaming its new save over the old
// one): uses add up, the older creation time and the newer last use win, and
// labels already on to are kept.
func copyMeta(from, to string) {
	src, dst := readMeta(from), readMeta(to)
	set := func(name, v string) { _ = syscall.Setxattr(to, name, []byte(v), 0) }
	if src.Uses > 0 {
		set(usesXattr, strconv.FormatInt(src.Uses+dst.Uses, 10))
	}
	if !src.LastUse.IsZero() && src.LastUse.After(dst.LastUse) {
		set(lastUseXattr, strconv.FormatInt(src.LastUse.Unix(), 10))
	}
	if !src.Created.IsZero() && (dst.Created.IsZero() || src.Created.Before(dst.Created)) {
		set(createdXattr, strconv.FormatInt(src.Created.Unix(), 10))
	}
	for l, v := range src.Labels {
		if _, ok := dst.Labels[l]; !ok {
			set(labelPrefix+l, v)
		}
	}
}

// countUse records a use of the stub at path: one more use, unless the last
// one was within window (a restore in several opens), and the time.
func countUse(path string, window time.Duration) {
	m := readMeta(path)
	if m.LastUse.IsZero() || time.Since(m.LastUse) >= window {
		_ = syscall.Setxattr(path, usesXattr, []byte(strconv.FormatInt(m.Uses+1, 10)), 0)
	}
	_ = syscall.Setxattr(path, lastUseXattr, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0)
}

// SetLabel sets (value != nil) or removes a label on the snapshot at rel.
func (k *FS) SetLabel(rel, label string, value *string) error {
	path, err := k.stubPath(rel)
	if err != nil {
		return err
	}
	if value == nil {
		if err := syscall.Removexattr(path, labelPrefix+label); err != nil && err != syscall.ENODATA {
			return err
		}
		return nil
	}
	return syscall.Setxattr(path, labelPrefix+label, []byte(*value), 0)
}

// stubPath maps a snapshot id (path relative to the mount) to its stub.
func (k *FS) stubPath(rel string) (string, error) {
	clean := filepath.Clean("/" + rel)[1:]
	if clean == "" || strings.HasPrefix(clean, "..") {
		return "", syscall.EINVAL
	}
	path := filepath.Join(k.tree, clean)
	if _, ok := stubSize(path); !ok {
		return "", syscall.ENOENT
	}
	return path, nil
}

// modelOf is the first path component of a snapshot id: the model's folder.
func modelOf(rel string) string {
	if i := strings.IndexByte(rel, '/'); i > 0 {
		return rel[:i]
	}
	return ""
}

// rel is a backing path relative to the tree.
func (k *FS) rel(path string) string {
	r, err := filepath.Rel(k.tree, path)
	if err != nil {
		return path
	}
	return r
}

// Unlink records the deletion of a snapshot as an event, with what it was.
func (n *Node) Unlink(ctx context.Context, name string) syscall.Errno {
	path := filepath.Join(n.backing(), name)
	size, stub := stubSize(path)
	var m Meta
	if stub {
		m = readMeta(path)
	}
	errno := n.LoopbackNode.Unlink(ctx, name)
	if errno == 0 && stub {
		rel := n.kfs.rel(path)
		fields := map[string]any{"model": modelOf(rel), "file": rel, "bytes": size, "uses": m.Uses}
		if !m.Created.IsZero() {
			fields["age_s"] = int64(time.Since(m.Created).Seconds())
		}
		if !m.LastUse.IsZero() {
			fields["idle_s"] = int64(time.Since(m.LastUse).Seconds())
		}
		n.kfs.ev.Emit("info", "delete", "deleted", fields)
		n.kfs.m.deletes.Inc(modelOf(rel))
	}
	return errno
}

// Rename carries a replaced snapshot's metadata onto the one replacing it.
func (n *Node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if np, ok := newParent.(*Node); ok && flags == 0 {
		from := filepath.Join(n.backing(), name)
		to := filepath.Join(np.backing(), newName)
		if _, ok := stubSize(from); ok {
			if _, ok := stubSize(to); ok {
				copyMeta(to, from)
			}
		}
	}
	return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
}
