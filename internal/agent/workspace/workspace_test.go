package workspace

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) Input {
	t.Helper()
	p := Payload{StackID: "11111111-1111-4111-8111-111111111111", VersionID: "22222222-2222-4222-8222-222222222222", ProjectName: "demo"}
	p.Config.Files = []File{{"compose.yaml", base64.StdEncoding.EncodeToString([]byte("services:\r\n  app: {image: alpine}\r\n"))}, {"sub/value.txt", base64.StdEncoding.EncodeToString([]byte("secret\n"))}}
	p.Config.ComposeFiles = []string{"compose.yaml"}
	raw, _ := json.Marshal(p)
	return Input{"33333333-3333-4333-8333-333333333333", "fingerprint", raw}
}
func TestOriginalBytesReuseAndDrift(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	in := fixture(t)
	p, err := Materialize(ctx, root, in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReadFile(ctx, p, "compose.yaml")
	if err != nil || string(b) != "services:\r\n  app: {image: alpine}\r\n" {
		t.Fatalf("bytes %q %v", b, err)
	}
	if _, err = Materialize(ctx, root, in); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(p.Dir, "extra"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if Verify(ctx, p) == nil {
		t.Fatal("extra entry accepted")
	}
	if _, err = Materialize(ctx, root, in); err == nil {
		t.Fatal("drift overwritten")
	}
}
func TestUnsafePathsAndPrefixRejected(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a\\b", "a:b", "a/../b", "a//b"} {
		t.Run(name, func(t *testing.T) {
			in := fixture(t)
			var p Payload
			json.Unmarshal(in.Payload, &p)
			p.Config.Files[1].Path = name
			in.Payload, _ = json.Marshal(p)
			if _, err := Materialize(context.Background(), t.TempDir(), in); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	in := fixture(t)
	var p Payload
	json.Unmarshal(in.Payload, &p)
	p.Config.Files[1].Path = "compose.yaml/nested"
	in.Payload, _ = json.Marshal(p)
	if _, err := Materialize(context.Background(), t.TempDir(), in); err == nil {
		t.Fatal("file prefix accepted")
	}
}
func TestSymlinkRejected(t *testing.T) {
	ctx := context.Background()
	p, err := Materialize(ctx, t.TempDir(), fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(p.Dir, "sub", "value.txt")
	if err = os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(t.TempDir(), "external"), target); err != nil {
		t.Skip("symlink privilege unavailable")
	}
	if Verify(ctx, p) == nil {
		t.Fatal("link accepted")
	}
}

func TestPackageContextAndControlledEnvCannotDrift(t *testing.T) {
	ctx := context.Background()
	p, err := Materialize(ctx, t.TempDir(), fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	p.Payload.ProjectName = "another"
	if Verify(ctx, p) == nil {
		t.Fatal("mutable context accepted")
	}
	p, err = Materialize(ctx, t.TempDir(), fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(p.OperationDir, "empty.env"), []byte("COMPOSE_FILE=outside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if Verify(ctx, p) == nil {
		t.Fatal("controlled env drift accepted")
	}
}

func TestExecutionContextIsFullyFrozen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	input := fixture(t)
	original, err := Materialize(ctx, root, input)
	if err != nil {
		t.Fatal(err)
	}
	other := input
	other.OperationID = "44444444-4444-4444-8444-444444444444"
	other.InputFingerprint = "other-fingerprint"
	otherPackage, err := Materialize(ctx, root, other)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Package){
		"operation_dir":                func(p *Package) { p.OperationDir = filepath.Join(t.TempDir(), "unchecked") },
		"other_existing_operation_dir": func(p *Package) { p.OperationDir = otherPackage.OperationDir },
		"operation_id":                 func(p *Package) { p.OperationID = other.OperationID },
		"input_fingerprint":            func(p *Package) { p.InputFingerprint = other.InputFingerprint },
		"all_operation_fields": func(p *Package) {
			p.OperationDir = otherPackage.OperationDir
			p.OperationID = other.OperationID
			p.InputFingerprint = other.InputFingerprint
		},
		"package_dir":  func(p *Package) { p.Dir = otherPackage.OperationDir },
		"workspace_id": func(p *Package) { p.WorkspaceID = "workspaces/another/package" },
		"manifest":     func(p *Package) { p.ManifestFingerprint = "changed" },
		"payload":      func(p *Package) { p.Payload.ProjectName = "changed" },
		"entries":      func(p *Package) { p.Entries[0].Size++ },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := original
			p.Entries = append([]Entry{}, p.Entries...)
			mutate(&p)
			if Verify(ctx, p) == nil {
				t.Fatal("changed execution context accepted")
			}
		})
	}
	if Verify(ctx, original) != nil {
		t.Fatal("unmodified execution context rejected")
	}
}
