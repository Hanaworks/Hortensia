package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"time"

	"docker-manager/internal/channel"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

type options struct {
	mode, listen, cert, key, tokenFile, nodeID, ledgerID, url, ca string
	duration                                                      time.Duration
}

func parse(args []string) (options, string) {
	var o options
	f := flag.NewFlagSet("channelprobe", flag.ContinueOnError)
	// flag's default error messages echo values, which may contain credentials.
	f.SetOutput(io.Discard)
	f.StringVar(&o.mode, "mode", "", "manager or agent")
	f.StringVar(&o.listen, "listen", "127.0.0.1:8443", "literal loopback address")
	f.StringVar(&o.cert, "cert", "", "manager TLS certificate file")
	f.StringVar(&o.key, "key", "", "manager TLS private key file")
	f.StringVar(&o.tokenFile, "token-file", "", "token file")
	f.StringVar(&o.nodeID, "node-id", "", "node UUID")
	f.StringVar(&o.ledgerID, "ledger-id", "", "ledger UUID")
	f.StringVar(&o.url, "url", "", "Agent WebSocket URL")
	f.StringVar(&o.ca, "ca", "", "trusted CA PEM file")
	f.DurationVar(&o.duration, "duration", 30*time.Second, "maximum probe duration")
	if f.Parse(args) != nil || f.NArg() != 0 || o.duration <= 0 || (o.mode != "manager" && o.mode != "agent") {
		return o, "invalid_arguments"
	}
	if o.mode == "manager" {
		host, port, err := net.SplitHostPort(o.listen)
		ip := net.ParseIP(host)
		p, portErr := strconv.Atoi(port)
		if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || p < 0 || p > 65535 {
			return o, "loopback_listen_required"
		}
		if o.cert == "" || o.key == "" {
			return o, "tls_files_required"
		}
	} else {
		u, err := url.Parse(o.url)
		if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/agent/v1/connect" {
			return o, "invalid_agent_url"
		}
	}
	if o.tokenFile == "" || o.nodeID == "" || o.ledgerID == "" {
		return o, "identity_files_required"
	}
	return o, ""
}

func run(ctx context.Context, args []string, out, diagnostic io.Writer) int {
	o, problem := parse(args)
	if problem != "" {
		fmt.Fprintln(diagnostic, problem)
		return 2
	}
	data, err := os.ReadFile(o.tokenFile)
	if err != nil {
		fmt.Fprintln(diagnostic, "invalid_token_file")
		return 2
	}
	token, err := channel.ParseToken(data)
	if err != nil {
		fmt.Fprintln(diagnostic, "invalid_token_file")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, o.duration)
	defer cancel()
	if o.mode == "manager" {
		return runManager(ctx, o, token, out, diagnostic)
	}
	return runAgent(ctx, o, token, out, diagnostic)
}

func runManager(ctx context.Context, o options, token string, out, diagnostic io.Writer) int {
	cert, err := tls.LoadX509KeyPair(o.cert, o.key)
	if err != nil {
		fmt.Fprintln(diagnostic, "invalid_tls_files")
		return 2
	}
	handler, err := channel.NewServer(channel.Config{NodeID: o.nodeID, LedgerID: o.ledgerID, Token: token})
	if err != nil {
		fmt.Fprintln(diagnostic, "invalid_identity")
		return 2
	}
	defer handler.Close()
	listener, err := net.Listen("tcp", o.listen)
	if err != nil {
		fmt.Fprintln(diagnostic, "listen_failed")
		return 1
	}
	defer listener.Close()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	defer server.Close()
	tlsListener := tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
	served := make(chan error, 1)
	go func() { served <- server.Serve(tlsListener) }()
	if err := json.NewEncoder(out).Encode(map[string]any{"event": "listening", "address": listener.Addr().String(), "node_id": o.nodeID, "ledger_id": o.ledgerID}); err != nil {
		_ = server.Close()
		<-served
		fmt.Fprintln(diagnostic, "report_failed")
		return 1
	}
	select {
	case <-ctx.Done():
		handler.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
		err = <-served
	case err = <-served:
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(diagnostic, "server_failed")
		return 1
	}
	return 0
}

func runAgent(ctx context.Context, o options, token string, out, diagnostic io.Writer) int {
	var roots *x509.CertPool
	if o.ca != "" {
		data, err := os.ReadFile(o.ca)
		if err != nil {
			fmt.Fprintln(diagnostic, "invalid_ca_file")
			return 2
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			fmt.Fprintln(diagnostic, "invalid_ca_file")
			return 2
		}
	}
	// Connect is called once. Authentication failures and disconnects end this probe.
	peer, err := channel.Connect(ctx, channel.ClientConfig{URL: o.url, NodeID: o.nodeID, LedgerID: o.ledgerID, Token: token, RootCAs: roots})
	if err != nil {
		if errors.Is(err, channel.ErrUnauthorized) {
			fmt.Fprintln(diagnostic, "unauthorized")
		} else {
			fmt.Fprintln(diagnostic, "connection_failed")
		}
		return 1
	}
	defer peer.Close()
	if err := json.NewEncoder(out).Encode(map[string]any{"event": "handshake", "node_id": o.nodeID, "ledger_id": o.ledgerID, "session_id": peer.SessionID, "reconcile_required": peer.ReconcileRequired}); err != nil {
		fmt.Fprintln(diagnostic, "report_failed")
		return 1
	}
	if err := peer.Run(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintln(diagnostic, "connection_closed")
		return 1
	}
	return 0
}
