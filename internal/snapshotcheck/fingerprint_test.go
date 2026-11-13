package snapshotcheck

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"math"
	"testing"
)

func newTestFingerprint(t *testing.T, component string) *Fingerprint {
	t.Helper()
	f, err := NewFingerprint(bytes.Repeat([]byte{7}, 32), component, "test-format", []string{"id", "key"}, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func TestFingerprintMatchesIndependentFraming(t *testing.T) {
	master := bytes.Repeat([]byte{7}, 32)
	var derive bytes.Buffer
	frame := func(dst *bytes.Buffer, tag byte, raw []byte) {
		dst.WriteByte(tag)
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(raw)))
		dst.Write(n[:])
		dst.Write(raw)
	}
	integer := func(n int64) []byte { var raw [8]byte; binary.BigEndian.PutUint64(raw[:], uint64(n)); return raw[:] }
	frame(&derive, 4, []byte("dm-snapshot-check-key-v1"))
	frame(&derive, 2, []byte("agent"))
	frame(&derive, 1, integer(2))
	frame(&derive, 2, []byte("id"))
	frame(&derive, 2, []byte("key"))
	h := hmac.New(sha256.New, master)
	h.Write(derive.Bytes())
	derived := h.Sum(nil)
	var root bytes.Buffer
	frame(&root, 4, []byte("dm-runtime-snapshot-semantic-v1"))
	frame(&root, 2, []byte("test-format"))
	frame(&root, 2, []byte("agent"))
	frame(&root, 1, integer(5))
	frame(&root, 1, integer(1))
	frame(&root, 1, integer(2))
	frame(&root, 2, []byte("id"))
	frame(&root, 2, []byte("key"))
	frame(&root, 4, []byte("row"))
	frame(&root, 0, nil)
	frame(&root, 1, integer(-1))
	frame(&root, 2, []byte("hello"))
	frame(&root, 3, []byte{0, 255})
	h = hmac.New(sha256.New, derived)
	h.Write(root.Bytes())
	want := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	f := newTestFingerprint(t, "agent")
	f.Marker("row")
	for _, v := range []any{nil, int64(-1), "hello", []byte{0, 255}} {
		if err := f.Value(v); err != nil {
			t.Fatal(err)
		}
	}
	if f.Sum() != want {
		t.Fatalf("framing differs: %s != %s", f.Sum(), want)
	}
}

func TestFingerprintSeparatesTypesBoundariesAndIdentity(t *testing.T) {
	values := []any{nil, int64(1), "1", []byte("1")}
	seen := map[string]bool{}
	for _, v := range values {
		f := newTestFingerprint(t, "agent")
		if err := f.Value(v); err != nil {
			t.Fatal(err)
		}
		sum := f.Sum()
		if sum == "" || seen[sum] {
			t.Fatal("type collision")
		}
		seen[sum] = true
	}
	a := newTestFingerprint(t, "agent")
	a.Marker("a")
	a.Value("bc")
	b := newTestFingerprint(t, "agent")
	b.Marker("ab")
	b.Value("c")
	if a.Sum() == b.Sum() {
		t.Fatal("boundary collision")
	}
	c := newTestFingerprint(t, "manager")
	c.Marker("a")
	c.Value("bc")
	if a.Sum() == c.Sum() {
		t.Fatal("component collision")
	}
	d, err := NewFingerprint(bytes.Repeat([]byte{7}, 32), "agent", "test-format", []string{"other", "key"}, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.Marker("a")
	d.Value("bc")
	if a.Sum() == d.Sum() {
		t.Fatal("identity collision")
	}
}

func TestFingerprintRejectsInvalidValuesAndClosedUse(t *testing.T) {
	if _, err := NewFingerprint([]byte{1}, "agent", "f", []string{"id"}, 5, 1); err == nil {
		t.Fatal("short key")
	}
	f := newTestFingerprint(t, "agent")
	before := f.Sum()
	for _, v := range []any{1.2, struct{}{}, uint64(math.MaxUint64)} {
		if err := f.Value(v); err == nil {
			t.Fatal("invalid value accepted")
		}
		if f.Sum() != before {
			t.Fatal("invalid value changed hash")
		}
	}
	f.Close()
	f.Marker("ignored")
	if err := f.Value("ignored"); err == nil || f.Sum() != "" {
		t.Fatal("closed fingerprint reusable")
	}
}
