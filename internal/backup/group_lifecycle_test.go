package backup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"docker-manager/internal/localstate"
)

func TestGroupLockChild(t *testing.T) {
	path := os.Getenv("DM_GROUP_LOCK_CHILD")
	if path == "" {
		return
	}
	lock, e := localstate.AcquireLock(path)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	if e = os.WriteFile(path+".ready", []byte("ready"), 0600); e != nil {
		t.Fatal(e)
	}
	end := time.Now().Add(8 * time.Second)
	for time.Now().Before(end) {
		if _, e = os.Stat(path + ".release"); e == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("controlled child was not released")
}
func groupChildLock(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestGroupLockChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "DM_GROUP_LOCK_CHILD="+path)
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if _, e := os.Stat(path + ".ready"); e == nil {
			return cmd
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("second process did not hold lock")
	return nil
}
func TestCreateGroupWaitsForMaterialsLockWithoutOutput(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	child := groupChildLock(t, req.Source.MaterialsLockFile)
	done := make(chan error, 1)
	go func() { _, e := CreateGroup(context.Background(), req, inspect); done <- e }()
	select {
	case e := <-done:
		t.Fatalf("did not wait for live OS lock: %v", e)
	case <-time.After(120 * time.Millisecond):
	}
	if _, e := os.Lstat(req.DataOut); !os.IsNotExist(e) {
		t.Fatal("created output while waiting")
	}
	if e := os.WriteFile(req.Source.MaterialsLockFile+".release", []byte("release"), 0600); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not resume after real process unlock")
	}
	if e := child.Wait(); e != nil {
		t.Fatal(e)
	}
}
func TestCreateGroupCancelsWaitingForMaterialsWithoutOutput(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	groupChildLock(t, req.Source.MaterialsLockFile)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Millisecond)
	defer cancel()
	_, e := CreateGroup(ctx, req, inspect)
	if !errors.Is(e, context.DeadlineExceeded) || !errors.Is(e, ErrMaterialsBusy) || errors.Is(e, ErrPartial) {
		t.Fatalf("waiting cancellation=%v", e)
	}
	if _, e := os.Lstat(req.DataOut); !os.IsNotExist(e) {
		t.Fatal("waiting cancellation created output")
	}
}
func TestCreateGroupFailuresPreservePublicationState(t *testing.T) {
	for _, kind := range []string{"snapshot", "write", "secret-rename", "data-rename", "final-sync", "cleanup", "cancel-before-data"} {
		t.Run(kind, func(t *testing.T) {
			req, inspect, _ := groupFixture(t)
			ops := realGroupOperations()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := errors.New("controlled I/O failure")
			switch kind {
			case "snapshot":
				ops.snapshot = func(context.Context, string, string) error { return injected }
			case "write":
				ops.write = func(string, []byte) error { return injected }
			case "secret-rename", "data-rename":
				original := ops.rename
				ops.rename = func(old, next string) error {
					if kind == "secret-rename" && filepath.Base(next) == "secret-manifest.json" || kind == "data-rename" && filepath.Base(next) == "manifest.json" {
						return injected
					}
					return original(old, next)
				}
			case "final-sync":
				original := ops.syncDir
				ops.syncDir = func(path string) error {
					if path == req.DataOut {
						if _, e := os.Stat(filepath.Join(path, "manifest.json")); e == nil {
							return injected
						}
					}
					return original(path)
				}
			case "cleanup":
				ops.closeLock = func(lock *localstate.Lock) error {
					if e := lock.Close(); e != nil {
						return e
					}
					return injected
				}
			case "cancel-before-data":
				original := ops.rename
				ops.rename = func(old, next string) error {
					e := original(old, next)
					if filepath.Base(next) == "secret-manifest.json" {
						cancel()
					}
					return e
				}
			}
			result, e := createGroup(ctx, req, inspect, ops)
			if e == nil {
				t.Fatal("injected failure was ignored")
			}
			_, manifestErr := os.Stat(filepath.Join(req.DataOut, "manifest.json"))
			switch kind {
			case "final-sync":
				if result.State != StatePublicationUncertain || !errors.Is(e, ErrPublicationUncertain) || errors.Is(e, ErrPartial) || manifestErr != nil {
					t.Fatalf("uncertain=%+v %v manifest=%v", result, e, manifestErr)
				}
			case "cleanup":
				if result.State != StateComplete || !errors.Is(e, ErrCleanup) || errors.Is(e, ErrPartial) || manifestErr != nil {
					t.Fatalf("cleanup=%+v %v manifest=%v", result, e, manifestErr)
				}
			default:
				if result.State != StatePartial || !errors.Is(e, ErrPartial) || !os.IsNotExist(manifestErr) {
					t.Fatalf("partial=%+v %v manifest=%v", result, e, manifestErr)
				}
			}
			// Failures must release the actual OS lock without removing its inode/name.
			lock, lockErr := localstate.AcquireLock(req.Source.MaterialsLockFile)
			if lockErr != nil {
				t.Fatal("failure leaked material lock", lockErr)
			}
			lock.Close()
		})
	}
}

func TestCreateGroupRejectsChangedMaterialBytesAndInspectorDatabase(t *testing.T) {
	for _, kind := range []string{"source-bytes", "snapshot-bytes", "snapshot-sidecar"} {
		t.Run(kind, func(t *testing.T) {
			req, inspect, _ := groupFixture(t)
			wrapped := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
				r, e := inspect(ctx, path, set)
				if e != nil {
					return r, e
				}
				switch kind {
				case "source-bytes":
					e = os.WriteFile(req.Source.Materials[1].Path, make([]byte, 32), 0600)
				case "snapshot-bytes":
					f, x := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
					if x != nil {
						return DomainReport{}, x
					}
					_, e = f.Write([]byte("changed"))
					f.Close()
				case "snapshot-sidecar":
					e = os.WriteFile(path+"-wal", []byte("unexpected"), 0600)
				}
				return r, e
			}
			result, e := CreateGroup(context.Background(), req, wrapped)
			if e == nil || result.State != StatePartial {
				t.Fatalf("unsafe change=%+v %v", result, e)
			}
			if _, e = os.Stat(filepath.Join(req.DataOut, "manifest.json")); !os.IsNotExist(e) {
				t.Fatal("published changed material")
			}
		})
	}
}

func TestCreateGroupCancellationAfterInspectionPreservesContextError(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wrapped := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		r, e := inspect(ctx, path, set)
		cancel()
		return r, e
	}
	result, e := CreateGroup(ctx, req, wrapped)
	if !errors.Is(e, context.Canceled) || !errors.Is(e, ErrPartial) || result.State != StatePartial {
		t.Fatalf("cancellation context lost: %+v %v", result, e)
	}
	if _, e = os.Stat(filepath.Join(req.DataOut, "manifest.json")); !os.IsNotExist(e) {
		t.Fatal("cancelled inspection published")
	}
}

func TestCreateGroupInspectorErrorPreservesCancellation(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := false
	wrapped := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		if _, err := inspect(ctx, path, set); err != nil {
			return DomainReport{}, err
		}
		entered = true
		cancel()
		return DomainReport{}, errors.New("controlled domain rejection after cancellation")
	}
	result, err := CreateGroup(ctx, req, wrapped)
	if !entered {
		t.Fatal("did not reach the failing inspector cancellation branch")
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrPartial) || result.State != StatePartial {
		t.Fatalf("failing inspector lost cancellation classification: state=%q err=%v", result.State, err)
	}
	if _, err := os.Stat(filepath.Join(req.DataOut, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("failing cancelled inspector published final manifest")
	}
}

func TestCreateGroupInspectorErrorPreservesDeadline(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered := false
	wrapped := func(ctx context.Context, path string, set MaterialSet) (DomainReport, error) {
		if _, err := inspect(ctx, path, set); err != nil {
			return DomainReport{}, err
		}
		entered = true
		<-ctx.Done()
		return DomainReport{}, errors.New("controlled domain rejection after deadline")
	}
	result, err := CreateGroup(ctx, req, wrapped)
	if !entered {
		t.Fatal("did not reach the failing inspector deadline branch")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrPartial) || result.State != StatePartial {
		t.Fatalf("failing inspector lost deadline classification: state=%q err=%v", result.State, err)
	}
	if _, err := os.Stat(filepath.Join(req.DataOut, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("failing timed-out inspector published final manifest")
	}
}

func TestCreateGroupNeverReusesCompletedOrPartialRoots(t *testing.T) {
	for _, kind := range []string{"complete", "partial"} {
		t.Run(kind, func(t *testing.T) {
			req, inspect, _ := groupFixture(t)
			ops := realGroupOperations()
			if kind == "partial" {
				ops.snapshot = func(context.Context, string, string) error { return ErrVerification }
			}
			result, e := createGroup(context.Background(), req, inspect, ops)
			if kind == "complete" && e != nil || kind == "partial" && (!errors.Is(e, ErrPartial) || result.State != StatePartial) {
				t.Fatalf("baseline=%+v %v", result, e)
			}
			before := verifyBackupTree(t, req.DataOut, req.SecretsOut)
			if _, e = CreateGroup(context.Background(), req, inspect); !errors.Is(e, ErrDestination) {
				t.Fatalf("reused roots=%v", e)
			}
			if !reflect.DeepEqual(before, verifyBackupTree(t, req.DataOut, req.SecretsOut)) {
				t.Fatal("rejected retry changed old group")
			}
			req.DataOut += "-retry"
			req.SecretsOut += "-retry"
			if result, e = CreateGroup(context.Background(), req, inspect); e != nil || result.State != StateComplete {
				t.Fatalf("new names failed=%+v %v", result, e)
			}
		})
	}
}

func TestCreateGroupSecondRootRacePreservesFirstAndConflictingDirectory(t *testing.T) {
	req, inspect, _ := groupFixture(t)
	ops := realGroupOperations()
	original := ops.syncDir
	placed := false
	ops.syncDir = func(path string) error {
		if path == req.DataOut && !placed {
			placed = true
			if e := os.Mkdir(req.SecretsOut, 0700); e != nil {
				return e
			}
			if e := os.WriteFile(filepath.Join(req.SecretsOut, "original"), []byte("keep-existing"), 0600); e != nil {
				return e
			}
		}
		return original(path)
	}
	result, e := createGroup(context.Background(), req, inspect, ops)
	if result.State != StatePartial || !errors.Is(e, ErrPartial) {
		t.Fatalf("second root collision=%+v %v", result, e)
	}
	if info, e := os.Stat(req.DataOut); e != nil || !info.IsDir() {
		t.Fatal("first root was removed")
	}
	raw, e := os.ReadFile(filepath.Join(req.SecretsOut, "original"))
	if e != nil || string(raw) != "keep-existing" {
		t.Fatal("conflicting directory changed")
	}
}
