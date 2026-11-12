// Package workspace preserves frozen package bytes. It never edits Compose input.
package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

var ErrDrift = errors.New("workspace_drift")
var ErrInput = errors.New("workspace_input_invalid")

type Input struct {
	OperationID, InputFingerprint string
	Payload                       []byte
}
type File struct {
	Path       string `json:"path"`
	ContentB64 string `json:"content_b64"`
}
type Config struct {
	Files        []File            `json:"files"`
	ComposeFiles []string          `json:"compose_files"`
	EnvFiles     []string          `json:"interpolation_env_files"`
	Env          map[string]string `json:"interpolation_env"`
	Profiles     []string          `json:"profiles"`
}
type Payload struct {
	OperationType        string   `json:"operation_type"`
	NodeID               string   `json:"node_id"`
	StackID              string   `json:"stack_id"`
	VersionID            string   `json:"version_id"`
	ProjectName          string   `json:"project_name"`
	Config               Config   `json:"config"`
	RequiredCapabilities []string `json:"required_capabilities"`
	ExecutionProfile     struct {
		EndpointID     string `json:"docker_endpoint_id"`
		PolicyRevision uint64 `json:"policy_revision"`
	} `json:"execution_profile"`
}
type Entry struct {
	Path   string `json:"path"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}
type Package struct {
	Dir, OperationDir, WorkspaceID, ManifestFingerprint, OperationID, InputFingerprint string
	Payload                                                                            Payload
	Entries                                                                            []Entry
	root, relative                                                                     string
	originals                                                                          map[string][]byte
	frozenPayload                                                                      []byte
	frozen                                                                             executionContext
}

// executionContext binds the public transport fields to the context that was
// materialized. In particular a different, otherwise valid operation directory
// must not substitute its HOME, Docker configuration, or empty interpolation env.
type executionContext struct {
	operationID, inputFingerprint, dir, operationDir, workspaceID, manifestFingerprint string
	entriesJSON                                                                        string
}

func SafePath(s string) bool {
	if s == "" || len(s) > 240 || !utf8.ValidString(s) || strings.ContainsAny(s, "\\:\x00") {
		return false
	}
	for _, v := range strings.Split(s, "/") {
		if v == "" || v == "." || v == ".." {
			return false
		}
	}
	return true
}
func safeID(s string) bool { return SafePath(s) && !strings.Contains(s, "/") && len(s) <= 128 }
func decode(root string, in Input) (Package, error) {
	p := Package{root: root, OperationID: in.OperationID, InputFingerprint: in.InputFingerprint, originals: map[string][]byte{}}
	d := json.NewDecoder(bytes.NewReader(in.Payload))
	d.DisallowUnknownFields()
	if d.Decode(&p.Payload) != nil {
		return p, ErrInput
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return p, ErrInput
	}
	if !filepath.IsAbs(root) || !safeID(in.OperationID) || !safeID(p.Payload.StackID) || !safeID(p.Payload.VersionID) || len(p.Payload.Config.Files) == 0 || len(p.Payload.Config.Files) > 128 || len(p.Payload.Config.ComposeFiles) == 0 {
		return p, ErrInput
	}
	total := 0
	for _, f := range p.Payload.Config.Files {
		if !SafePath(f.Path) {
			return p, ErrInput
		}
		if _, ok := p.originals[f.Path]; ok {
			return p, ErrInput
		}
		b, err := base64.StdEncoding.Strict().DecodeString(f.ContentB64)
		if err != nil || len(b) > 1<<20 {
			return p, ErrInput
		}
		total += len(b)
		if total > 4<<20 {
			return p, ErrInput
		}
		sum := sha256.Sum256(b)
		p.Entries = append(p.Entries, Entry{f.Path, len(b), hex.EncodeToString(sum[:])})
		p.originals[f.Path] = b
	}
	for a := range p.originals {
		for b := range p.originals {
			if strings.HasPrefix(a, b+"/") {
				return p, ErrInput
			}
		}
	}
	for _, list := range [][]string{p.Payload.Config.ComposeFiles, p.Payload.Config.EnvFiles} {
		seen := map[string]bool{}
		for _, n := range list {
			if _, ok := p.originals[n]; !ok || seen[n] {
				return p, ErrInput
			}
			seen[n] = true
		}
	}
	m, _ := json.Marshal(p.Entries)
	p.frozenPayload, _ = json.Marshal(p.Payload)
	sum := sha256.Sum256(m)
	p.ManifestFingerprint = base64.RawURLEncoding.EncodeToString(sum[:])
	p.WorkspaceID = path.Join("workspaces", p.Payload.StackID, p.Payload.VersionID, "package")
	p.relative = p.WorkspaceID
	p.Dir = filepath.Join(root, filepath.FromSlash(p.WorkspaceID))
	p.OperationDir = filepath.Join(root, "operations", in.OperationID)
	p.frozen = executionContext{operationID: p.OperationID, inputFingerprint: p.InputFingerprint, dir: p.Dir, operationDir: p.OperationDir, workspaceID: p.WorkspaceID, manifestFingerprint: p.ManifestFingerprint, entriesJSON: string(m)}
	return p, nil
}
func Materialize(ctx context.Context, stateDir string, in Input) (Package, error) {
	p, err := decode(stateDir, in)
	if err != nil {
		return p, err
	}
	fs, err := openSecure(stateDir)
	if err != nil {
		return p, err
	}
	defer fs.Close()
	if err = ctx.Err(); err != nil {
		return p, err
	}
	parent := path.Dir(p.relative)
	if err = fs.MkdirAll(parent); err != nil {
		return p, err
	}
	exists, err := fs.Exists(p.relative)
	if err != nil {
		return p, ErrDrift
	}
	if !exists {
		var nonce [12]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return p, err
		}
		stage := path.Join(parent, ".staging-"+in.OperationID+"-"+hex.EncodeToString(nonce[:]))
		if err = fs.MkdirAll(stage); err != nil {
			return p, err
		}
		for _, e := range p.Entries {
			if err = ctx.Err(); err != nil {
				return p, err
			}
			dst := path.Join(stage, e.Path)
			if err = fs.MkdirAll(path.Dir(dst)); err != nil {
				return p, err
			}
			if err = fs.WriteExclusive(dst, p.originals[e.Path]); err != nil {
				return p, err
			}
		}
		if err = fs.Rename(stage, p.relative); err != nil {
			return p, err
		}
	}
	if err = verifyFS(ctx, fs, p); err != nil {
		return p, err
	}
	op := path.Join("operations", in.OperationID)
	for _, d := range []string{op, path.Join(op, "home"), path.Join(op, "docker-config")} {
		if err = fs.MkdirAll(d); err != nil {
			return p, err
		}
	}
	manifest, _ := json.Marshal(p.Entries)
	for n, b := range map[string][]byte{"manifest.json": manifest, "empty.env": {}} {
		name := path.Join(op, n)
		exists, err = fs.Exists(name)
		if err != nil {
			return p, ErrDrift
		}
		if exists {
			actual, e := fs.ReadFile(name)
			if e != nil || !bytes.Equal(actual, b) {
				return p, ErrDrift
			}
		} else if err = fs.WriteExclusive(name, b); err != nil {
			return p, err
		}
	}
	return p, nil
}
func verifyFS(ctx context.Context, fs *secureFS, p Package) error {
	names, err := fs.Files(p.relative)
	if err != nil {
		return ErrDrift
	}
	expected := map[string]bool{}
	for _, e := range p.Entries {
		expected[e.Path] = true
		for d := path.Dir(e.Path); d != "."; d = path.Dir(d) {
			expected[d+"/"] = true
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		b, e2 := fs.ReadFile(path.Join(p.relative, e.Path))
		if e2 != nil || !bytes.Equal(b, p.originals[e.Path]) {
			return ErrDrift
		}
	}
	if len(names) != len(expected) {
		return ErrDrift
	}
	for _, n := range names {
		if !expected[n] {
			return ErrDrift
		}
	}
	return nil
}
func Verify(ctx context.Context, p Package) error {
	f := p.frozen
	if f.operationID == "" || p.OperationID != f.operationID || p.InputFingerprint != f.inputFingerprint || p.Dir != f.dir || p.OperationDir != f.operationDir || p.WorkspaceID != f.workspaceID || p.ManifestFingerprint != f.manifestFingerprint {
		return ErrDrift
	}
	current, _ := json.Marshal(p.Payload)
	if !bytes.Equal(current, p.frozenPayload) || p.WorkspaceID != p.relative {
		return ErrDrift
	}
	m, _ := json.Marshal(p.Entries)
	if string(m) != f.entriesJSON {
		return ErrDrift
	}
	sum := sha256.Sum256(m)
	if p.ManifestFingerprint != base64.RawURLEncoding.EncodeToString(sum[:]) {
		return ErrDrift
	}
	fs, err := openSecure(p.root)
	if err != nil {
		return ErrDrift
	}
	defer fs.Close()
	if p.relative == "" || p.Dir != filepath.Join(p.root, filepath.FromSlash(p.relative)) {
		return ErrDrift
	}
	if err = verifyFS(ctx, fs, p); err != nil {
		return err
	}
	manifest, _ := json.Marshal(p.Entries)
	op := path.Join("operations", f.operationID)
	if p.OperationDir != filepath.Join(p.root, filepath.FromSlash(op)) {
		return ErrDrift
	}
	for name, expected := range map[string][]byte{"manifest.json": manifest, "empty.env": {}} {
		actual, e := fs.ReadFile(path.Join(op, name))
		if e != nil || !bytes.Equal(actual, expected) {
			return ErrDrift
		}
	}
	for _, name := range []string{"home", "docker-config"} {
		entries, e := fs.Files(path.Join(op, name))
		if e != nil || len(entries) != 0 {
			return ErrDrift
		}
	}
	return nil
}
func ReadFile(ctx context.Context, p Package, name string) ([]byte, error) {
	if _, ok := p.originals[name]; !ok {
		return nil, ErrInput
	}
	if err := Verify(ctx, p); err != nil {
		return nil, err
	}
	return bytes.Clone(p.originals[name]), nil
}
func Names(p Package) []string {
	n := make([]string, 0, len(p.originals))
	for s := range p.originals {
		n = append(n, s)
	}
	sort.Strings(n)
	return n
}
