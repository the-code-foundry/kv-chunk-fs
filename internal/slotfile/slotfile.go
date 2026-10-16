// Package slotfile reads the token list from a llama.cpp slot snapshot and
// builds prefix trees of token lists across snapshots.
//
// File layout (llama.cpp llama_state_seq_save_file, state format 4):
//
//	u32 magic 'ggsq'   u32 version (4)   u32 n   int32 tokens[n]   state...
//
// llama-server stores its own envelope in tokens[] (server_tokens::serialize):
//
//	-1 (LLAMA_TOKEN_NULL marker)   1 (envelope version)   count   tokens[count]
//	media_count   media...
//
// Older servers stored the plain token list. Anything else is reported as an
// error, never a panic.
package slotfile

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	magic           = 0x67677371 // 'ggsq'
	stateVersion    = 4
	envelopeVersion = 1
	tokenNull       = -1
)

// Header is what a snapshot says about its tokens.
type Header struct {
	StateVersion uint32
	Envelope     string  // "server-tokens v1" or "plain"
	Tokens       []int32 // the conversation's tokens
	Media        int     // media chunks (images); tokens past the first are placeholders
}

// Parse reads the header of a snapshot of the given size.
func Parse(r io.ReaderAt, size int64) (*Header, error) {
	var head [12]byte
	if size < int64(len(head)) {
		return nil, fmt.Errorf("truncated header (%d bytes)", size)
	}
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return nil, fmt.Errorf("reading header: %v", err)
	}
	if m := binary.LittleEndian.Uint32(head[0:]); m != magic {
		return nil, fmt.Errorf("not a llama.cpp slot file (magic 0x%08x)", m)
	}
	v := binary.LittleEndian.Uint32(head[4:])
	if v != stateVersion {
		return nil, fmt.Errorf("format version %d, expected %d", v, stateVersion)
	}
	n := int64(binary.LittleEndian.Uint32(head[8:]))
	if 12+4*n > size {
		return nil, fmt.Errorf("token count %d exceeds the file (%d bytes)", n, size)
	}
	raw := make([]byte, 4*n)
	if _, err := r.ReadAt(raw, 12); err != nil && err != io.EOF {
		return nil, fmt.Errorf("reading %d tokens: %v", n, err)
	}
	toks := make([]int32, n)
	for i := range toks {
		toks[i] = int32(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	h := &Header{StateVersion: v}
	if len(toks) == 0 || toks[0] != tokenNull {
		h.Envelope, h.Tokens = "plain", toks
		return h, nil
	}
	if len(toks) < 3 {
		return nil, fmt.Errorf("truncated server tokens envelope")
	}
	if ev := toks[1]; ev != envelopeVersion {
		return nil, fmt.Errorf("server tokens envelope version %d, expected %d", ev, envelopeVersion)
	}
	count := int64(uint32(toks[2]))
	if 3+count > int64(len(toks)) {
		return nil, fmt.Errorf("server tokens envelope: %d tokens declared, %d present", count, len(toks)-3)
	}
	h.Envelope, h.Tokens = fmt.Sprintf("server-tokens v%d", envelopeVersion), toks[3:3+count]
	if rest := toks[3+count:]; len(rest) > 0 {
		h.Media = int(uint32(rest[0]))
	}
	return h, nil
}
