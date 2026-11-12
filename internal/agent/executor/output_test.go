package executor

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

func TestOutputPreservesSplitUTF8AndDrains(t *testing.T) {
	c := &outputCollector{}
	w := streamWriter{c, 0}
	b := []byte("甲乙")
	for _, v := range b {
		w.Write([]byte{v})
	}
	streamWriter{c, 1}.Write([]byte{255})
	w.Write(bytes.Repeat([]byte("界"), OutputLimit))
	out, truncated := c.finish()
	if !truncated || len(out) > OutputLimit || !utf8.Valid(out) || !bytes.HasPrefix(out, []byte("甲乙�")) {
		t.Fatalf("invalid bounded output len=%d truncated=%v", len(out), truncated)
	}
	if n, err := w.Write(bytes.Repeat([]byte("x"), 3<<20)); err != nil || n != 3<<20 {
		t.Fatal("writer did not drain")
	}
}
