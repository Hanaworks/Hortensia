package agenttransport

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"docker-manager/internal/agentwire"
)

// Delay a real accepted socket's first WebSocket write (welcome), without
// changing Manager production code or the Store's binding transaction.
type reviewDelayConn struct {
	net.Conn
	entered, release chan struct{}
	once             sync.Once
}

func (c *reviewDelayConn) Write(b []byte) (int, error) {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Conn.Write(b)
}

type reviewDelayWriter struct {
	http.ResponseWriter
	entered, release chan struct{}
}

func (w reviewDelayWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return c, rw, err
	}
	wrapped := &reviewDelayConn{Conn: c, entered: w.entered, release: w.release}
	rw.Writer.Reset(wrapped)
	return wrapped, rw, nil
}

func TestReviewLateWelcomeMustNotStopNewerCurrentConnection(t *testing.T) {
	f := newTransportPair(t)
	f.server.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var selectFirst sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slow := false
		selectFirst.Do(func() { slow = true })
		if slow {
			f.h.ServeHTTP(reviewDelayWriter{w, entered, release}, r)
		} else {
			f.h.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.server = server
	firstDone := make(chan *agentwire.Peer, 1)
	firstError := make(chan error, 1)
	go func() {
		p, _, err := agentwire.Dial(f.ctx, agentwire.ClientConfig{URL: "wss" + strings.TrimPrefix(server.URL, "https") + "/agent/v1/connect", Token: f.secret, Hello: f.hello(t, f.node), RootCAs: server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs})
		if err != nil {
			firstError <- err
			return
		}
		firstDone <- p
	}()
	select {
	case <-entered:
	case err := <-firstError:
		t.Fatal(err)
	case <-f.ctx.Done():
		t.Fatal("first welcome not blocked")
	}
	second, b := f.dial(t)
	f.recover(t, second, b)
	unblock()
	select {
	case p := <-firstDone:
		defer p.Close()
	case err := <-firstError:
		t.Fatal(err)
	case <-f.ctx.Done():
		t.Fatal("late welcome did not finish")
	}
	// If the stale handler stops the newer peer, its exact-session cleanup also
	// clears the actual current Store binding. The new peer must stay current.
	time.Sleep(100 * time.Millisecond)
	state, err := f.m.ReadRuntimeConnectionState(context.Background(), b)
	if err != nil {
		t.Fatalf("late older welcome destroyed newer completed binding: %v", err)
	}
	if state.Frozen || state.Recovery.Status != "completed" {
		t.Fatalf("newer binding changed: %+v", state)
	}
}
