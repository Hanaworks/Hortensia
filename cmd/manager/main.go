package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"docker-manager/internal/manager/operations"
	"docker-manager/internal/manager/service"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, service.ErrConfig):
		return "configuration_invalid"
	case errors.Is(err, service.ErrFiles):
		return "private_files_invalid"
	case errors.Is(err, service.ErrLocked):
		return "already_running"
	case errors.Is(err, service.ErrInitialized):
		return "initialization_materials_exist"
	case errors.Is(err, service.ErrTLS):
		return "tls_invalid"
	case errors.Is(err, service.ErrPassword):
		return "password_input_invalid"
	case errors.Is(err, operations.ErrKey):
		return "key_mismatch"
	case errors.Is(err, operations.ErrIdentity):
		return "identity_mismatch"
	case errors.Is(err, operations.ErrSchema):
		return "database_format_invalid"
	}
	var e *operations.ServiceError
	if errors.As(err, &e) {
		return e.Code
	}
	return "manager_unavailable"
}

func run(ctx context.Context, args []string, input *os.File, out, errOut io.Writer) int {
	usage := func() int {
		fmt.Fprintln(errOut, "用法: manager init --config <绝对路径> --admin <账号> [--password-stdin] | manager serve --config <绝对路径>")
		return 2
	}
	if len(args) == 0 || (args[0] != "init" && args[0] != "serve") {
		return usage()
	}
	flags := flag.NewFlagSet("manager", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "")
	var admin *string
	var fromStdin *bool
	if args[0] == "init" {
		admin = flags.String("admin", "", "")
		fromStdin = flags.Bool("password-stdin", false, "")
	}
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || *configPath == "" {
		return usage()
	}
	fail := func(err error) int { fmt.Fprintln(errOut, "Manager:", errorCode(err)); return 1 }
	c, err := service.LoadConfig(*configPath)
	if err != nil {
		return fail(err)
	}
	if args[0] == "init" {
		if *admin == "" {
			return usage()
		}
		var password []byte
		if *fromStdin {
			password, err = service.ReadPasswordLineContext(ctx, input)
		} else {
			fmt.Fprint(errOut, "管理员口令: ")
			password, err = service.ReadHiddenPasswordContext(ctx, input)
			fmt.Fprintln(errOut)
			if err == nil {
				fmt.Fprint(errOut, "再次输入口令: ")
				confirmation, confirmErr := service.ReadHiddenPasswordContext(ctx, input)
				fmt.Fprintln(errOut)
				if confirmErr != nil {
					err = confirmErr
				} else if subtle.ConstantTimeCompare(password, confirmation) != 1 {
					err = service.ErrPassword
				}
				clear(confirmation)
			}
		}
		defer clear(password)
		if err != nil {
			return fail(err)
		}
		identity, err := service.Initialize(ctx, c, *admin, string(password))
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "Manager已初始化:", identity.ManagerID)
		return 0
	}
	runtime, err := service.Start(c)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintln(out, "Manager HTTPS已启动:", c.ExternalOrigin)
	select {
	case <-ctx.Done():
	case <-runtime.Done():
	}
	closing, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := runtime.Close(closing); err != nil {
		return fail(err)
	}
	fmt.Fprintln(out, "Manager已关闭")
	return 0
}
