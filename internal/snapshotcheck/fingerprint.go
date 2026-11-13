package snapshotcheck

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash"
	"math"
)

type Fingerprint struct{ h hash.Hash }

func frame(h hash.Hash, tag byte, raw []byte) {
	_, _ = h.Write([]byte{tag})
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(raw)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(raw)
}
func framedInteger(h hash.Hash, n int64) {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], uint64(n))
	frame(h, 1, raw[:])
}

func NewFingerprint(master []byte, component, format string, stableIDs []string, schema, protocol int) (*Fingerprint, error) {
	if len(master) != 32 || (component != "agent" && component != "manager") || format == "" || len(stableIDs) == 0 || schema < 1 || protocol < 1 {
		return nil, ErrInput
	}
	for _, id := range stableIDs {
		if id == "" {
			return nil, ErrInput
		}
	}
	derive := hmac.New(sha256.New, master)
	frame(derive, 4, []byte("dm-snapshot-check-key-v1"))
	frame(derive, 2, []byte(component))
	framedInteger(derive, int64(len(stableIDs)))
	for _, id := range stableIDs {
		frame(derive, 2, []byte(id))
	}
	key := derive.Sum(nil)
	h := hmac.New(sha256.New, key)
	clear(key)
	frame(h, 4, []byte("dm-runtime-snapshot-semantic-v1"))
	frame(h, 2, []byte(format))
	frame(h, 2, []byte(component))
	framedInteger(h, int64(schema))
	framedInteger(h, int64(protocol))
	framedInteger(h, int64(len(stableIDs)))
	for _, id := range stableIDs {
		frame(h, 2, []byte(id))
	}
	return &Fingerprint{h: h}, nil
}
func (f *Fingerprint) Marker(value string) {
	if f != nil && f.h != nil {
		frame(f.h, 4, []byte(value))
	}
}
func (f *Fingerprint) Value(value any) error {
	if f == nil || f.h == nil {
		return ErrInput
	}
	switch v := value.(type) {
	case nil:
		frame(f.h, 0, nil)
	case int64:
		framedInteger(f.h, v)
	case uint64:
		if v > math.MaxInt64 {
			return ErrLimit
		}
		framedInteger(f.h, int64(v))
	case string:
		frame(f.h, 2, []byte(v))
	case []byte:
		frame(f.h, 3, v)
	default:
		return ErrInput
	}
	return nil
}
func (f *Fingerprint) Sum() string {
	if f == nil || f.h == nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(f.h.Sum(nil))
}
func (f *Fingerprint) Close() {
	if f != nil {
		f.h = nil
	}
}
