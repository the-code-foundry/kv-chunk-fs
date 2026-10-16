package slotfile

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func file(version uint32, toks []int32, state int) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(magic))
	binary.Write(&b, binary.LittleEndian, version)
	binary.Write(&b, binary.LittleEndian, uint32(len(toks)))
	binary.Write(&b, binary.LittleEndian, toks)
	b.Write(make([]byte, state))
	return b.Bytes()
}

func envelope(toks ...int32) []int32 {
	return append(append([]int32{-1, 1, int32(len(toks))}, toks...), 0) // + media count 0
}

func parse(b []byte) (*Header, error) { return Parse(bytes.NewReader(b), int64(len(b))) }

func TestServerEnvelope(t *testing.T) {
	h, err := parse(file(4, envelope(151644, 8948, 198), 100))
	if err != nil {
		t.Fatal(err)
	}
	if h.Envelope != "server-tokens v1" || len(h.Tokens) != 3 || h.Tokens[0] != 151644 || h.Media != 0 {
		t.Fatalf("%+v", h)
	}
}

func TestPlainTokens(t *testing.T) {
	h, err := parse(file(4, []int32{1, 2, 3}, 10))
	if err != nil || h.Envelope != "plain" || len(h.Tokens) != 3 {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestErrorsNotPanics(t *testing.T) {
	good := file(4, envelope(1, 2), 10)
	badMagic := append([]byte{}, good...)
	badMagic[0] = 'X'
	tooMany := file(4, []int32{1, 2}, 0)
	binary.LittleEndian.PutUint32(tooMany[8:], 1000)
	badEnvelope := file(4, []int32{-1, 7, 1, 5}, 10)
	shortEnvelope := file(4, []int32{-1, 1, 50, 5}, 10)
	for name, c := range map[string]struct {
		b    []byte
		want string
	}{
		"empty":          {nil, "truncated header"},
		"short":          {good[:7], "truncated header"},
		"magic":          {badMagic, "not a llama.cpp slot file"},
		"version":        {file(5, envelope(1), 10), "format version 5, expected 4"},
		"count":          {tooMany, "exceeds the file"},
		"envelope":       {badEnvelope, "envelope version 7"},
		"envelope count": {shortEnvelope, "50 tokens declared"},
	} {
		_, err := parse(c.b)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
}

func TestTree(t *testing.T) {
	sys := []int32{1, 2, 3, 4, 5}
	items := []Item{
		{"a", append(append([]int32{}, sys...), 10, 11)},
		{"b", append(append([]int32{}, sys...), 10, 12, 13)},
		{"c", append(append([]int32{}, sys...), 20)},
		{"d", []int32{9, 9}},
		{"e", append([]int32{}, sys...)}, // ends inside the shared run
	}
	root := Build(items)
	if root.Snapshots != 5 || root.End != 0 || len(root.Children) != 2 {
		t.Fatalf("root %+v", root)
	}
	var sysNode *Node
	for _, c := range root.Children {
		if c.Snapshots == 4 {
			sysNode = c
		}
	}
	if sysNode == nil || sysNode.End != 5 || len(sysNode.Ends) != 1 || sysNode.Ends[0] != "e" || len(sysNode.Children) != 2 {
		t.Fatalf("shared system prompt node %+v", sysNode)
	}
	sum := Summary(root)
	if sum[0] != (Shared{Tokens: 5, Snapshots: 4}) {
		t.Fatalf("summary %+v", sum)
	}
	found := false
	for _, s := range sum {
		if s == (Shared{Tokens: 6, Snapshots: 2}) { // a and b share 1,2,3,4,5,10
			found = true
		}
	}
	if !found {
		t.Fatalf("a/b branch missing: %+v", sum)
	}
}
