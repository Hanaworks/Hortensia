//go:build linux

package executor

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	if os.Getenv("M1_PROCESS_FIXTURE") != "1" {
		return
	}
	mode := os.Getenv("M1_PROCESS_MODE")
	if mode == "cooperative" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		os.WriteFile(os.Getenv("M1_PROCESS_READY"), []byte("ready"), 0600)
		<-ch
		os.Exit(0)
	}
	if mode == "ignore" {
		signal.Ignore(syscall.SIGTERM)
		cmd := exec.Command(os.Args[0])
		cmd.Env = append([]string{}, os.Environ()...)
		for i, v := range cmd.Env {
			if strings.HasPrefix(v, "M1_PROCESS_MODE=") {
				cmd.Env[i] = "M1_PROCESS_MODE=descendant"
			}
		}
		if cmd.Start() != nil {
			os.Exit(3)
		}
		os.WriteFile(os.Getenv("M1_PROCESS_READY"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600)
		cmd.Wait()
		os.Exit(0)
	}
	if mode == "descendant" {
		signal.Ignore(syscall.SIGTERM)
		for {
			os.WriteFile(os.Getenv("M1_PROCESS_PULSE"), []byte(time.Now().String()), 0600)
			time.Sleep(20 * time.Millisecond)
		}
	}
	os.Exit(4)
}
func helperCommand(t *testing.T, mode string, timeout time.Duration) (Command, string, string) {
	t.Helper()
	dir := t.TempDir()
	exe, _ := os.Executable()
	ready, pulse := filepath.Join(dir, "ready"), filepath.Join(dir, "pulse")
	return Command{Program: exe, Dir: dir, Env: []string{"M1_PROCESS_FIXTURE=1", "M1_PROCESS_MODE=" + mode, "M1_PROCESS_READY=" + ready, "M1_PROCESS_PULSE=" + pulse}, Timeout: timeout}, ready, pulse
}
func waitReady(t *testing.T, path string) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper never entered subprocess")
}
func TestInterruptedExitZeroRemainsInterrupted(t *testing.T) {
	cmd, ready, _ := helperCommand(t, "cooperative", time.Minute)
	p, err := Start(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, ready)
	p.Interrupt("canceled")
	o := p.Wait()
	if o.Outcome != "interrupted" || o.InterruptReason != "canceled" || o.ExitCode == nil || *o.ExitCode != 0 {
		t.Fatalf("wrong process fact %#v", o)
	}
	if p.Wait().FinishedAt != o.FinishedAt {
		t.Fatal("Wait did not preserve original fact")
	}
}
func TestDeadlineKillsIgnoringProcessGroup(t *testing.T) {
	cmd, ready, pulse := helperCommand(t, "ignore", time.Second)
	start := time.Now()
	p, err := Start(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, ready)
	o := p.Wait()
	if o.Outcome != "interrupted" || o.InterruptReason != "deadline" || time.Since(start) < 10*time.Second || time.Since(start) > 16*time.Second {
		t.Fatalf("wrong deadline fact %#v elapsed=%s", o, time.Since(start))
	}
	before, err := os.ReadFile(pulse)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	after, _ := os.ReadFile(pulse)
	if string(before) != string(after) {
		t.Fatal("descendant survived process-group kill")
	}
}
