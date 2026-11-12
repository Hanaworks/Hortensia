package backup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Directory junction creation does not need the symlink privilege; exercise the
// actual Windows reparse rejection rather than skipping all link validation.
func TestSnapshotRejectsWindowsJunctionAncestors(t *testing.T) {
	_, source, root := fixture(t, true)
	junction := filepath.Join(root, "junction")
	output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", junction, root).CombinedOutput()
	if err != nil {
		if strings.Contains(strings.ToLower(string(output)), "access is denied") || strings.Contains(strings.ToLower(string(output)), "privilege is not held") {
			t.Skipf("junction creation capability denied: %v (%s)", err, output)
		}
		t.Fatalf("create controlled junction: %v (%s)", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(junction); err != nil {
			t.Errorf("remove junction only: %v", err)
		}
	})
	if err := Snapshot(context.Background(), filepath.Join(junction, "source.sqlite"), filepath.Join(root, "bad-source", "copy.sqlite")); !errors.Is(err, ErrInput) {
		t.Fatalf("junction source ancestor = %v", err)
	}
	if err := Snapshot(context.Background(), source, filepath.Join(junction, "bad-target", "copy.sqlite")); !errors.Is(err, ErrDestination) {
		t.Fatalf("junction target ancestor = %v", err)
	}
	for _, name := range []string{"bad-source", "bad-target"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("junction validation wrote staging %s: %v", name, err)
		}
	}
}
