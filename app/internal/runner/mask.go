package runner

import (
	"bytes"
	"sort"
	"strings"
)

// Masked replaces secret values in job logs.
const Masked = "[MASKED]"

// minMaskLen is the shortest value that is masked. Masking every "1" or
// "on" in a log would destroy it and protect nothing.
const minMaskLen = 4

// masker replaces secret values in a stream of log bytes. A value can be
// split across two writes, so the end of the buffer that could be the
// start of a value is held back until the next write or Close.
type masker struct {
	patterns [][]byte // longest first, so a value wins over its own lines
	pending  []byte
}

// newMasker masks each value, and each line of a multi-line value (such
// as an SSH key printed line by line).
func newMasker(values []string) *masker {
	seen := map[string]bool{}
	var pats []string
	add := func(s string) {
		if len(s) >= minMaskLen && !seen[s] {
			seen[s] = true
			pats = append(pats, s)
		}
	}
	for _, v := range values {
		add(v)
		if strings.ContainsAny(v, "\r\n") {
			for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }) {
				add(strings.TrimSpace(line))
			}
		}
	}
	sort.Slice(pats, func(i, j int) bool { return len(pats[i]) > len(pats[j]) })
	m := &masker{}
	for _, p := range pats {
		m.patterns = append(m.patterns, []byte(p))
	}
	return m
}

// write takes new bytes and returns the bytes that are safe to send.
func (m *masker) write(p []byte) []byte {
	if len(m.patterns) == 0 {
		return append([]byte(nil), p...)
	}
	m.pending = append(m.pending, p...)
	out := m.replace(m.pending)
	hold := m.partialSuffix(out)
	safe := append([]byte(nil), out[:len(out)-hold]...)
	m.pending = append(m.pending[:0], out[len(out)-hold:]...)
	return safe
}

// close returns whatever is still held back.
func (m *masker) close() []byte {
	out := m.replace(m.pending)
	m.pending = nil
	return out
}

// replace masks every complete occurrence of a pattern.
func (m *masker) replace(b []byte) []byte {
	var out []byte
	for i := 0; i < len(b); {
		matched := false
		for _, p := range m.patterns {
			if bytes.HasPrefix(b[i:], p) {
				out = append(out, Masked...)
				i += len(p)
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, b[i])
			i++
		}
	}
	return out
}

// partialSuffix returns the length of the longest end of b that is the
// start of a pattern, which must wait for more bytes.
func (m *masker) partialSuffix(b []byte) int {
	longest := 0
	for _, p := range m.patterns {
		for n := min(len(p)-1, len(b)); n > longest; n-- {
			if bytes.Equal(b[len(b)-n:], p[:n]) {
				longest = n
				break
			}
		}
	}
	return longest
}
