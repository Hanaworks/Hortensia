//go:build !linux

package executor

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMutationExecutionExplicitlyUnsupported(t *testing.T) {
	exe, _ := os.Executable()
	_, err := Start(context.Background(), Command{Program: exe, Dir: t.TempDir(), Env: []string{}, Timeout: time.Second})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported platform started mutation process: %v", err)
	}
}
