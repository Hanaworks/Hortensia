package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"docker-manager/internal/manager/httpapi"
	"docker-manager/internal/manager/operations"
	"docker-manager/internal/protocol"
)

var ErrServe = errors.New("manager HTTP service failed")

type activeHTTP struct {
	handler  http.Handler
	mu       sync.Mutex
	closed   bool
	requests sync.WaitGroup
}

func (a *activeHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		id, _ := protocol.NewID()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"category": "unavailable", "code": "internal_error", "message": "服务正在关闭", "request_id": id, "field_errors": []any{}, "retry_action": "same_request"}})
		return
	}
	a.requests.Add(1)
	a.mu.Unlock()
	defer a.requests.Done()
	a.handler.ServeHTTP(w, r)
}
func (a *activeHTTP) wait() { a.mu.Lock(); a.closed = true; a.mu.Unlock(); a.requests.Wait() }

type Runtime struct {
	server   *http.Server
	store    *operations.Store
	lock     *fileLock
	active   *activeHTTP
	cancel   context.CancelFunc
	done     chan struct{}
	serveErr error
	once     sync.Once
	closeErr error
}

func (r *Runtime) Done() <-chan struct{} { return r.done }

func tlsMaterial(c Config) (tls.Certificate, error) {
	for _, path := range []string{c.TLSCertFile, c.TLSKeyFile} {
		if err := rejectLinks(path); err != nil {
			return tls.Certificate{}, ErrTLS
		}
	}
	read := func(path string, secret bool) ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, ErrTLS
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 || secret && !privateMode(info, false) {
			return nil, ErrTLS
		}
		raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		if err != nil || len(raw) > 1<<20 {
			return nil, ErrTLS
		}
		return raw, nil
	}
	certPEM, err := read(c.TLSCertFile, false)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := read(c.TLSKeyFile, true)
	if err != nil {
		return tls.Certificate{}, err
	}
	defer clear(keyPEM)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(cert.Certificate) == 0 {
		return tls.Certificate{}, ErrTLS
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, ErrTLS
	}
	host, err := originHost(c.ExternalOrigin)
	if err != nil || leaf.VerifyHostname(host) != nil || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return tls.Certificate{}, ErrTLS
	}
	cert.Leaf = leaf
	return cert, nil
}

func Start(c Config) (*Runtime, error) {
	if err := validateRuntimeConfig(c); err != nil {
		return nil, err
	}
	certificate, err := tlsMaterial(c)
	if err != nil {
		return nil, err
	}
	if err := checkWebDir(c.WebDir); err != nil {
		return nil, err
	}
	dataDir := filepath.Dir(c.DatabaseFile)
	if err := checkPrivateDir(dataDir); err != nil {
		return nil, err
	}
	if err := rejectLinks(c.DatabaseFile); err != nil {
		return nil, err
	}
	info, err := os.Lstat(c.DatabaseFile)
	if err != nil || !info.Mode().IsRegular() || !privateMode(info, false) {
		return nil, ErrFiles
	}
	lock, err := acquireFileLock(filepath.Join(dataDir, ".manager.lock"))
	if err != nil {
		return nil, err
	}
	identity, keys, err := loadIdentity(c)
	if err != nil {
		lock.Close()
		return nil, err
	}
	defer clearKeys(&keys)
	store, err := operations.OpenService(context.Background(), c.DatabaseFile, identity.StoreIdentity(), keys)
	if err != nil {
		lock.Close()
		return nil, err
	}
	api, err := httpapi.New(httpapi.Config{Store: store, ExternalOrigin: c.ExternalOrigin})
	if err != nil {
		store.Close()
		lock.Close()
		return nil, ErrConfig
	}
	handler, err := newWebHandler(c.WebDir, api)
	if err != nil {
		store.Close()
		lock.Close()
		return nil, err
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		store.Close()
		lock.Close()
		return nil, ErrServe
	}
	base, cancel := context.WithCancel(context.Background())
	active := &activeHTTP{handler: handler}
	server := &http.Server{Handler: active, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}, BaseContext: func(net.Listener) context.Context { return base }, ErrorLog: log.New(io.Discard, "", 0)}
	r := &Runtime{server: server, store: store, lock: lock, active: active, cancel: cancel, done: make(chan struct{})}
	go func() {
		err := server.ServeTLS(listener, "", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.serveErr = ErrServe
		}
		close(r.done)
	}()
	return r, nil
}

func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		if err := r.server.Shutdown(ctx); err != nil {
			_ = r.server.Close()
		}
		r.cancel()
		r.active.wait()
		<-r.done
		storeErr := r.store.Close()
		lockErr := r.lock.Close()
		if storeErr != nil || lockErr != nil {
			r.closeErr = ErrFiles
		} else if r.serveErr != nil {
			r.closeErr = r.serveErr
		}
	})
	return r.closeErr
}

func Serve(ctx context.Context, c Config) error {
	r, err := Start(c)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-r.done:
	}
	closing, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return r.Close(closing)
}
