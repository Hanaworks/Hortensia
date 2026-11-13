package backup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/localstate"
)

type groupStamp struct {
	info  os.FileInfo
	bytes int64
	sha   string
}

func groupPathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}
func groupCheckDir(path string) error {
	if !localFile(path) || localstate.CheckPrivateDir(path) != nil {
		return ErrInput
	}
	f, e := os.Open(path)
	if e != nil {
		return ErrInput
	}
	info, e := f.Stat()
	if e == nil {
		e = groupOpenedPath(path, f, info, true)
	}
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		return ErrInput
	}
	return nil
}
func groupOpenPrivate(path string) (*os.File, os.FileInfo, error) {
	if !localFile(path) || localstate.RejectLinks(path) != nil || groupCheckDir(filepath.Dir(path)) != nil {
		return nil, nil, ErrInput
	}
	before, e := os.Lstat(path)
	if e != nil || !before.Mode().IsRegular() || localstate.IsReparse(before) || !localstate.PrivateMode(before, false) {
		return nil, nil, ErrInput
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, nil, ErrInput
	}
	info, e := f.Stat()
	if e != nil || !os.SameFile(before, info) || !info.Mode().IsRegular() || !localstate.PrivateMode(info, false) || groupOpenedPath(path, f, info, false) != nil {
		f.Close()
		return nil, nil, ErrInput
	}
	return f, info, nil
}
func groupRead(path string, limit int64) (raw []byte, err error) {
	f, info, e := groupOpenPrivate(path)
	if e != nil {
		return nil, e
	}
	defer func() {
		if f.Close() != nil {
			clear(raw)
			raw = nil
			err = ErrInput
		}
	}()
	if info.Size() > limit {
		return nil, ErrInput
	}
	raw, e = io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(raw)) > limit {
		clear(raw)
		return nil, ErrInput
	}
	return raw, nil
}
func groupHash(ctx context.Context, path string) (stamp groupStamp, err error) {
	f, info, e := groupOpenPrivate(path)
	if e != nil {
		return stamp, e
	}
	defer func() {
		if f.Close() != nil {
			stamp = groupStamp{}
			err = errors.Join(err, ErrInput)
		}
	}()
	hash := sha256.New()
	buf := make([]byte, 64<<10)
	defer clear(buf)
	var size int64
	for {
		if e = ctx.Err(); e != nil {
			return stamp, e
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
			size += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return stamp, ErrInput
		}
	}
	after, e := f.Stat()
	if e != nil || !os.SameFile(info, after) || size != info.Size() || after.Size() != info.Size() || groupOpenedPath(path, f, after, false) != nil {
		return stamp, ErrVerification
	}
	return groupStamp{info: info, bytes: size, sha: base64.RawURLEncoding.EncodeToString(hash.Sum(nil))}, nil
}
func groupSameFile(a, b groupStamp) bool {
	return os.SameFile(a.info, b.info) && a.bytes == b.bytes && a.sha == b.sha
}
func groupNoSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, e := os.Lstat(path + suffix); !os.IsNotExist(e) {
			return ErrVerification
		}
	}
	return nil
}
func groupClosedHash(ctx context.Context, path string) (groupStamp, error) {
	if groupNoSidecars(path) != nil {
		return groupStamp{}, ErrVerification
	}
	return groupHash(ctx, path)
}
func groupTree(root string, allowed []string) error {
	if groupCheckDir(root) != nil {
		return ErrVerification
	}
	names := make(map[string]bool, len(allowed))
	for _, p := range allowed {
		names[p] = false
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ErrVerification
		}
		if path == root {
			return nil
		}
		relative, e := filepath.Rel(root, path)
		if e != nil {
			return ErrVerification
		}
		name := filepath.ToSlash(relative)
		if _, ok := names[name]; !ok {
			return ErrVerification
		}
		names[name] = true
		if entry.IsDir() {
			return groupCheckDir(path)
		}
		f, _, e := groupOpenPrivate(path)
		if e != nil {
			return ErrVerification
		}
		if f.Close() != nil {
			return ErrVerification
		}
		return nil
	})
	if err != nil {
		return ErrVerification
	}
	for _, present := range names {
		if !present {
			return ErrVerification
		}
	}
	return nil
}
func groupDataTree(final bool) []string {
	paths := []string{"sqlite", "sqlite/state.db", "config.json"}
	if final {
		paths = append(paths, "manifest.json")
	}
	return paths
}
func groupSecretTree(c Component, final bool) []string {
	paths := []string{"identity.json", "keys"}
	for _, role := range groupRoles(c) {
		if role != RoleIdentity {
			paths = append(paths, groupMaterialPath(role))
		}
	}
	if final {
		paths = append(paths, "secret-manifest.json")
	}
	return paths
}
func groupWrite(path string, raw []byte) error {
	if !localFile(path) || localstate.RejectLinks(path) != nil || groupCheckDir(filepath.Dir(path)) != nil || !groupSafeLeaf(filepath.Base(path)) {
		return ErrDestination
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return ErrDestination
	}
	n, w := f.Write(raw)
	s := f.Sync()
	c := f.Close()
	if w != nil || s != nil || c != nil || n != len(raw) {
		return ErrDestination
	}
	return nil
}
func groupPublish(ctx context.Context, path string, raw []byte, rename func(string, string) error) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".manifest-*")
	if e != nil {
		return ErrDestination
	}
	temporary := f.Name()
	if f.Chmod(0600) != nil {
		f.Close()
		return ErrDestination
	}
	n, w := f.Write(raw)
	s := f.Sync()
	c := f.Close()
	if w != nil || s != nil || c != nil || n != len(raw) {
		return ErrDestination
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if rename(temporary, path) != nil {
		return ErrDestination
	}
	return nil
}
func groupAcquire(ctx context.Context, path string) (*localstate.Lock, error) {
	delay := 50 * time.Millisecond
	for {
		if e := ctx.Err(); e != nil {
			return nil, errors.Join(ErrMaterialsBusy, e)
		}
		if _, e := os.Lstat(path); e == nil {
			f, _, e := groupOpenPrivate(path)
			if e != nil {
				return nil, ErrInput
			}
			if f.Close() != nil {
				return nil, ErrInput
			}
		} else if !os.IsNotExist(e) {
			return nil, ErrInput
		}
		lock, e := localstate.AcquireLock(path)
		if e == nil {
			f, _, checkErr := groupOpenPrivate(path)
			if checkErr == nil {
				checkErr = f.Close()
			}
			if checkErr != nil {
				lock.Close()
				return nil, ErrInput
			}
			return lock, nil
		}
		if !errors.Is(e, localstate.ErrLocked) {
			return nil, ErrInput
		}
		if e = pause(ctx, delay); e != nil {
			return nil, errors.Join(ErrMaterialsBusy, e)
		}
		delay = nextDelay(delay)
	}
}
func groupPrepareRequest(req GroupRequest) error {
	if groupRoles(req.Source.Component) == nil || validateGroupConfig(req.Source.Component, req.Source.ConfigJSON) != nil || req.Source.Creator.ApplicationVersion == "" || req.Source.Creator.BuildRevision != nil && *req.Source.Creator.BuildRevision == "" {
		return ErrInput
	}
	if _, e := normalizeExternal(req.Source.Component, req.Source.ExternalMaterials); e != nil {
		return e
	}
	source := req.Source.DatabaseFile
	dir := filepath.Dir(source)
	if !localFile(source) || validateSource(source) != nil || groupCheckDir(dir) != nil {
		return ErrInput
	}
	f, _, e := groupOpenPrivate(source)
	if e != nil {
		return ErrInput
	}
	if f.Close() != nil {
		return ErrInput
	}
	if groupPathKey(req.Source.MaterialsLockFile) != groupPathKey(filepath.Join(dir, ".materials.lock")) {
		return ErrInput
	}
	paths := []string{source, source + "-wal", source + "-shm", source + "-journal", filepath.Join(dir, "."+string(req.Source.Component)+".lock"), req.Source.MaterialsLockFile}
	seenRoles := map[MaterialRole]bool{}
	if len(req.Source.Materials) != 3 {
		return ErrInput
	}
	for _, m := range req.Source.Materials {
		if seenRoles[m.Role] {
			return ErrInput
		}
		valid := false
		for _, r := range groupRoles(req.Source.Component) {
			valid = valid || r == m.Role
		}
		if !valid {
			return ErrInput
		}
		seenRoles[m.Role] = true
		paths = append(paths, m.Path)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if !localFile(p) || localstate.RejectLinks(p) != nil || groupCheckDir(filepath.Dir(p)) != nil || !groupSafeLeaf(filepath.Base(p)) {
			return ErrInput
		}
		key := groupPathKey(p)
		if seen[key] {
			return ErrInput
		}
		seen[key] = true
		if _, e := os.Lstat(p); e == nil {
			f, _, e := groupOpenPrivate(p)
			if e != nil {
				return ErrInput
			}
			if f.Close() != nil {
				return ErrInput
			}
		} else if !os.IsNotExist(e) {
			return ErrInput
		}
	}
	data, secrets := groupPathKey(req.DataOut), groupPathKey(req.SecretsOut)
	sep := string(filepath.Separator)
	if data == secrets || strings.HasPrefix(data, secrets+sep) || strings.HasPrefix(secrets, data+sep) {
		return ErrDestination
	}
	for _, p := range []string{req.DataOut, req.SecretsOut} {
		if !localFile(p) || !groupSafeLeaf(filepath.Base(p)) || localstate.RejectLinks(p) != nil || groupCheckDir(filepath.Dir(p)) != nil || seen[groupPathKey(p)] {
			return ErrDestination
		}
		if _, e := os.Lstat(p); !os.IsNotExist(e) {
			return ErrDestination
		}
	}
	return nil
}
func groupCapture(s Source) (MaterialSet, error) {
	set := MaterialSet{ConfigJSON: bytesClone(s.ConfigJSON)}
	sources := append([]MaterialSource{}, s.Materials...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].Role < sources[j].Role })
	for _, source := range sources {
		limit := groupIdentityLimit
		if source.Role != RoleIdentity {
			limit = 32
		}
		raw, e := groupRead(source.Path, limit)
		if e != nil || len(raw) == 0 || source.Role != RoleIdentity && len(raw) != 32 {
			clear(raw)
			groupClear(&set)
			return MaterialSet{}, ErrInput
		}
		set.Files = append(set.Files, Material{Role: source.Role, Raw: raw})
	}
	return set, nil
}
func bytesClone(raw []byte) []byte { return append([]byte{}, raw...) }
func groupClear(set *MaterialSet) {
	for _, m := range set.Files {
		clear(m.Raw)
	}
	clear(set.ConfigJSON)
}
