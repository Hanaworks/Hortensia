package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/netip"
	"sync"
	"time"
)

type failureBucket struct {
	failures, pending int
	expires           time.Time
}
type failureLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*failureBucket
	capacity int
}

func newFailureLimiter(capacity int) *failureLimiter {
	return &failureLimiter{buckets: make(map[string]*failureBucket), capacity: capacity}
}
func secondsUntil(expiry, now time.Time) int {
	n := int(expiry.Sub(now).Seconds() + 0.999999999)
	if n < 1 {
		return 1
	}
	return n
}
func (l *failureLimiter) acquire(now time.Time, keys []string) (func(bool), int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if !now.Before(b.expires) && b.pending == 0 {
			delete(l.buckets, k)
		}
	}
	missing := 0
	retry := 0
	for _, k := range keys {
		b := l.buckets[k]
		if b == nil {
			missing++
			continue
		}
		if b.failures+b.pending >= 10 {
			if n := secondsUntil(b.expires, now); n > retry {
				retry = n
			}
		}
	}
	if retry != 0 {
		return nil, retry
	}
	if len(l.buckets)+missing > l.capacity {
		return nil, 60
	}
	held := make([]*failureBucket, 0, len(keys))
	for _, k := range keys {
		b := l.buckets[k]
		if b == nil {
			b = &failureBucket{expires: now.Add(time.Minute)}
			l.buckets[k] = b
		}
		b.pending++
		held = append(held, b)
	}
	var once sync.Once
	return func(failed bool) {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			for _, b := range held {
				b.pending--
				if failed {
					b.failures++
				}
			}
		})
	}, 0
}
func budgetKey(kind, value string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + value))
	return hex.EncodeToString(sum[:])
}
func peerIP(remote string) string {
	host, _, e := net.SplitHostPort(remote)
	if e != nil {
		host = remote
	}
	ip, e := netip.ParseAddr(host)
	if e != nil {
		return "unknown"
	}
	return ip.Unmap().WithZone("").String()
}
