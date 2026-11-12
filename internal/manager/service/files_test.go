package service

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestPrivateFilesNeverOverwriteExistingMaterial(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "key")
	if err := writeExclusive(path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusive(path, []byte("changed")); err == nil {
		t.Fatal("overwrote identity material")
	}
	raw, err := readPrivateFile(path, 8)
	if err != nil || string(raw) != "original" {
		t.Fatalf("read changed material: %q %v", raw, err)
	}
	if _, err := readPrivateFile(path, 7); err == nil {
		t.Fatal("read ignored budget")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := readPrivateFile(path, 8); err == nil {
			t.Fatal("world-readable secret accepted")
		}
	}
}

func TestPrivateFilesRejectLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("Windows account cannot create symlinks; Linux link acceptance remains required")
		}
		t.Fatal(err)
	}
	if err := ensurePrivateDir(link); err == nil {
		t.Fatal("linked private directory accepted")
	}
	if err := writeExclusive(filepath.Join(link, "key"), []byte("secret")); err == nil {
		t.Fatal("linked parent accepted")
	}
	if _, err := os.Stat(filepath.Join(target, "key")); !os.IsNotExist(err) {
		t.Fatal("link check happened after write")
	}
}

func lockCommand(t *testing.T, path, mode string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestManagerLockHelper$")
	cmd.Env = append(os.Environ(), "DM_LOCK_TEST_PATH="+path, "DM_LOCK_TEST_MODE="+mode)
	return cmd, cancel
}

func TestManagerLockUsesOperatingSystemProcessLifetime(t *testing.T) {
	dir := t.TempDir()
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".manager.lock")
	lock, err := acquireFileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	probe, cancel := lockCommand(t, path, "probe")
	err = probe.Run()
	cancel()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 42 {
		t.Fatalf("second process took lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	holder, cancel := lockCommand(t, path, "hold")
	defer cancel()
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("child did not acquire: %q %v", line, err)
	}
	if second, err := acquireFileLock(path); err == nil {
		second.Close()
		t.Fatal("parent acquired child's lock")
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	reopened, err := acquireFileLock(path)
	if err != nil {
		t.Fatal("dead process left lock held:", err)
	}
	defer reopened.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("lock file should remain")
	}
}

func TestManagerLockHelper(t *testing.T) {
	path := os.Getenv("DM_LOCK_TEST_PATH")
	if path == "" {
		return
	}
	lock, err := acquireFileLock(path)
	if err != nil {
		os.Exit(42)
	}
	defer lock.Close()
	if os.Getenv("DM_LOCK_TEST_MODE") == "hold" {
		_, _ = os.Stdout.WriteString("locked\n")
		for {
			time.Sleep(time.Hour)
		}
	}
}
