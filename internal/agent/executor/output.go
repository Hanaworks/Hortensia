package executor

import (
	"bytes"
	"sync"
	"unicode/utf8"
)

const OutputLimit = 1 << 20

// Streams keep separate decoder tails; the collector merges completed chunks
// in lock-acquisition order and continues accepting writes after truncation.
type outputCollector struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
	pending   [2][]byte
}
type streamWriter struct {
	c      *outputCollector
	stream int
}

func (w streamWriter) Write(p []byte) (int, error) {
	n := len(p)
	c := w.c
	c.mu.Lock()
	defer c.mu.Unlock()
	b := append(c.pending[w.stream], p...)
	c.pending[w.stream] = nil
	for len(b) > 0 {
		if !utf8.FullRune(b) {
			c.pending[w.stream] = bytes.Clone(b)
			break
		}
		r, size := utf8.DecodeRune(b)
		c.appendRune(r)
		b = b[size:]
	}
	return n, nil
}
func (c *outputCollector) appendRune(r rune) {
	b := utf8.AppendRune(nil, r)
	if len(c.data)+len(b) <= OutputLimit && !c.truncated {
		c.data = append(c.data, b...)
	} else {
		c.truncated = true
	}
}
func (c *outputCollector) finish() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.pending {
		for len(c.pending[i]) > 0 {
			r, n := utf8.DecodeRune(c.pending[i])
			c.appendRune(r)
			c.pending[i] = c.pending[i][n:]
		}
	}
	return bytes.Clone(c.data), c.truncated
}
