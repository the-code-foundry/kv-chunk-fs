package slotfile

import (
	"sort"
)

// Node is a run of tokens shared by every snapshot below it.
type Node struct {
	Start     int     `json:"start"`               // offset of this run in the token lists
	End       int     `json:"end"`                 // tokens from the beginning to the end of this run
	Snapshots int     `json:"snapshots"`           // snapshots sharing tokens [0, End)
	Ends      []string `json:"ends,omitempty"`     // snapshots whose token list ends exactly at End
	Children  []*Node `json:"children,omitempty"` // where the snapshots diverge
}

// Item is one snapshot's token list.
type Item struct {
	ID     string
	Tokens []int32
}

// Build returns the prefix tree of the items: a compressed (radix) tree where
// each node is the longest run all snapshots below it share. Built by sorting
// and splitting, so memory is proportional to the input, not to the token count
// times the number of snapshots.
func Build(items []Item) *Node {
	sorted := append([]Item(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return less(sorted[i].Tokens, sorted[j].Tokens) })
	if len(sorted) == 0 {
		return &Node{}
	}
	return build(sorted, 0)
}

func less(a, b []int32) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// build: items sorted, all sharing tokens [0, start).
func build(items []Item, start int) *Node {
	// in lexicographic order the group's common prefix is that of first and last
	first, last := items[0].Tokens, items[len(items)-1].Tokens
	end := start
	for end < len(first) && end < len(last) && first[end] == last[end] {
		end++
	}
	n := &Node{Start: start, End: end, Snapshots: len(items)}
	var rest []Item
	for _, it := range items {
		if len(it.Tokens) == end {
			n.Ends = append(n.Ends, it.ID)
		} else {
			rest = append(rest, it)
		}
	}
	// split the others by their next token; sorted, so groups are contiguous
	for i := 0; i < len(rest); {
		j := i + 1
		for j < len(rest) && rest[j].Tokens[end] == rest[i].Tokens[end] {
			j++
		}
		n.Children = append(n.Children, build(rest[i:j], end))
		i = j
	}
	// a single child with no snapshot ending here is the same run: merge
	if len(n.Children) == 1 && len(n.Ends) == 0 {
		c := n.Children[0]
		c.Start = start
		return c
	}
	return n
}

// Shared is a prefix that several snapshots have in common.
type Shared struct {
	Tokens    int `json:"tokens"`    // length of the shared prefix
	Snapshots int `json:"snapshots"` // how many share it
}

// Summary lists, for every branch point with more than one snapshot, how many
// snapshots share how many leading tokens - "20 conversations share their first
// 7,131 tokens" - largest groups first.
func Summary(root *Node) []Shared {
	var out []Shared
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Snapshots > 1 && n.End > 0 {
			out = append(out, Shared{Tokens: n.End, Snapshots: n.Snapshots})
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Snapshots != out[j].Snapshots {
			return out[i].Snapshots > out[j].Snapshots
		}
		return out[i].Tokens > out[j].Tokens
	})
	return out
}
