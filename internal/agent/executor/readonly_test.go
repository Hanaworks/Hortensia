package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func init() {
	if os.Getenv("M1_READONLY_FIXTURE") != "1" {
		return
	}
	var group sync.WaitGroup
	for _, stream := range []*os.File{os.Stdout, os.Stderr} {
		group.Add(1)
		go func(f *os.File) {
			defer group.Done()
			chunk := bytes.Repeat([]byte("x"), 32768)
			for i := 0; i < 128; i++ {
				if _, err := f.Write(chunk); err != nil {
					os.Exit(3)
				}
			}
		}(stream)
	}
	group.Wait()
	os.Exit(0)
}
func TestReadOnlyOutputBudgetStillDrainsBothPipes(t *testing.T) {
	exe, _ := os.Executable()
	start := time.Now()
	_, err := RunReadOnly(context.Background(), Command{Program: exe, Dir: t.TempDir(), Env: []string{"M1_READONLY_FIXTURE=1"}, Timeout: 5 * time.Second}, 1024)
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("expected bounded rejection, got %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("output drain did not complete promptly")
	}
}
