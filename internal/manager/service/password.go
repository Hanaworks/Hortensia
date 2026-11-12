package service

import (
	"context"
	"errors"
	"io"
	"os"
)

var ErrPassword = errors.New("password input unavailable or invalid")

// ReadPasswordLine reads a single UTF-8 line without trimming the password.
// Only a line-ending LF and its optional preceding CR are removed. It is
// available for explicitly requested stdin input; callers must not select it
// automatically when terminal protection is unavailable.
func ReadPasswordLine(input io.Reader) ([]byte, error) {
	var raw []byte
	one := make([]byte, 1)
	for {
		n, err := input.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				if len(raw) > 0 && raw[len(raw)-1] == '\r' {
					raw = raw[:len(raw)-1]
				}
				return raw, nil
			}
			raw = append(raw, one[0])
			if len(raw) > 513 {
				clear(raw)
				return nil, ErrPassword
			}
		}
		if err == io.EOF {
			return raw, nil
		}
		if err != nil {
			clear(raw)
			return nil, ErrPassword
		}
	}
}

func ReadHiddenPassword(input *os.File) ([]byte, error) {
	return ReadHiddenPasswordContext(context.Background(), input)
}

// The command exits after cancellation and does not reuse this input stream.
// An OS console read may outlive the context, but cannot delay mode restoration
// or command exit. An unclaimed result is cleared by the reading goroutine.
func ReadPasswordLineContext(ctx context.Context, input io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result)
	go func() {
		raw, err := ReadPasswordLine(input)
		select {
		case done <- result{raw, err}:
		case <-ctx.Done():
			clear(raw)
		}
	}()
	select {
	case r := <-done:
		return r.raw, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func ReadHiddenPasswordContext(ctx context.Context, input *os.File) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	restore, err := disableEcho(input)
	if err != nil {
		return nil, ErrPassword
	}
	raw, readErr := ReadPasswordLineContext(ctx, input)
	restoreErr := restore()
	if readErr != nil || restoreErr != nil {
		clear(raw)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrPassword
	}
	return raw, nil
}
