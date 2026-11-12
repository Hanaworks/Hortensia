package main

import (
	"bufio"
	"context"
	"docker-manager/internal/agent/service"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func run(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agent init|enroll|rotate-credentials|serve --config <absolute path>")
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(out, "docker-manager-agent m1-dev")
		return nil
	}
	f := flag.NewFlagSet("agent "+args[0], flag.ContinueOnError)
	f.SetOutput(out)
	config := f.String("config", "", "absolute configuration path")
	node := f.String("node-id", "", "Manager registered node UUID")
	stdin := f.Bool("token-stdin", false, "read enrollment token from standard input")
	if e := f.Parse(args[1:]); e != nil {
		if e == flag.ErrHelp {
			return nil
		}
		return e
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if args[0] != "init" && *node != "" || args[0] != "enroll" && *stdin {
		return fmt.Errorf("flag is not supported by this command")
	}
	switch args[0] {
	case "init", "enroll", "rotate-credentials", "serve":
	default:
		return fmt.Errorf("unknown command")
	}
	c, e := service.LoadConfig(*config)
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "init":
		_, e = service.Initialize(ctx, c, *node)
	case "enroll":
		if !*stdin {
			return fmt.Errorf("enroll requires --token-stdin")
		}
		line, e2 := bufio.NewReader(io.LimitReader(in, 258)).ReadString('\n')
		if e2 != nil && e2 != io.EOF || len(line) > 45 {
			return fmt.Errorf("token input invalid")
		}
		token := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if strings.ContainsAny(token, "\r\n ") {
			return fmt.Errorf("token input invalid")
		}
		e = service.Enroll(ctx, c, token)
	case "rotate-credentials":
		e = service.RotateCredentials(ctx, c)
	case "serve":
		e = service.Serve(ctx, c)
	}
	if e == nil {
		fmt.Fprintln(out, "Agent command completed.")
	}
	return e
}
func main() {
	if e := run(os.Args[1:], os.Stdin, os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
