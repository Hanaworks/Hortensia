package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"time"

	"docker-manager/internal/localstate"
	"docker-manager/internal/protocol"
)

// groupOperations is a per-call internal I/O boundary. It is never exposed as
// configuration; tests can fail an actual publication stage without globals.
type groupOperations struct {
	snapshot  func(context.Context, string, string) error
	write     func(string, []byte) error
	syncDir   func(string) error
	rename    func(string, string) error
	closeLock func(*localstate.Lock) error
}

func realGroupOperations() groupOperations {
	return groupOperations{Snapshot, groupWrite, localstate.SyncDirectory, groupRenameNoReplace, func(l *localstate.Lock) error { return l.Close() }}
}

// CreateGroup captures live SQLite state and complete identity/key material in
// two new private roots. The final data manifest is the publication boundary.
func CreateGroup(ctx context.Context, request GroupRequest, inspect DomainInspector) (GroupResult, error) {
	return createGroup(ctx, request, inspect, realGroupOperations())
}
func createGroup(ctx context.Context, request GroupRequest, inspect DomainInspector, ops groupOperations) (result GroupResult, err error) {
	if ctx == nil || inspect == nil {
		return result, ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	if e := ctx.Err(); e != nil {
		return result, e
	}
	if e := groupPrepareRequest(request); e != nil {
		return result, e
	}
	lock, e := groupAcquire(ctx, request.Source.MaterialsLockFile)
	if e != nil {
		return result, e
	}
	defer func() {
		if e := ops.closeLock(lock); e != nil {
			err = errors.Join(err, ErrCleanup)
		}
		if err != nil && result.State == StatePartial {
			err = errors.Join(ErrPartial, err)
		}
	}()
	if e = groupPrepareRequest(request); e != nil {
		return result, e
	}
	materials, e := groupCapture(request.Source)
	if e != nil {
		return result, e
	}
	defer groupClear(&materials)
	sourceStamps := make([]groupStamp, len(request.Source.Materials))
	for i, source := range request.Source.Materials {
		sourceStamps[i], e = groupHash(ctx, source.Path)
		if e != nil {
			return result, e
		}
	}
	id, e := protocol.NewID()
	if e != nil {
		return result, ErrInput
	}
	result.BackupID = id
	result.DataManifest = filepath.Join(request.DataOut, "manifest.json")
	result.SecretManifest = filepath.Join(request.SecretsOut, "secret-manifest.json")
	started := time.Now().UTC().Format(time.RFC3339Nano)
	for _, dir := range []string{request.DataOut, request.SecretsOut, filepath.Join(request.SecretsOut, "keys")} {
		if e = ctx.Err(); e != nil {
			return result, e
		}
		if os.Mkdir(dir, 0700) != nil {
			return result, ErrDestination
		}
		result.State = StatePartial
		if groupCheckDir(dir) != nil || ops.syncDir(dir) != nil || ops.syncDir(filepath.Dir(dir)) != nil {
			return result, ErrDestination
		}
	}
	database := filepath.Join(request.DataOut, "sqlite", "state.db")
	if e = ops.snapshot(ctx, request.Source.DatabaseFile, database); e != nil {
		return result, e
	}
	if e = ops.write(filepath.Join(request.DataOut, "config.json"), materials.ConfigJSON); e != nil {
		return result, e
	}
	for _, m := range materials.Files {
		if e = ctx.Err(); e != nil {
			return result, e
		}
		if e = ops.write(filepath.Join(request.SecretsOut, filepath.FromSlash(groupMaterialPath(m.Role))), m.Raw); e != nil {
			return result, e
		}
	}
	before, e := groupClosedHash(ctx, database)
	if e != nil {
		return result, e
	}
	report, e := inspect(ctx, database, materials)
	if e != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, errors.Join(ErrVerification, e)
	}
	after, e := groupClosedHash(ctx, database)
	if e != nil || !groupSameFile(before, after) {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, ErrVerification
	}
	report, e = normalizeReport(report)
	if e != nil || report.Component != request.Source.Component {
		return result, ErrVerification
	}
	current, e := groupCapture(request.Source)
	if e != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, ErrMaterialsChanged
	}
	defer groupClear(&current)
	for i, source := range request.Source.Materials {
		stamp, e := groupHash(ctx, source.Path)
		if e != nil || !groupSameFile(sourceStamps[i], stamp) {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, ErrMaterialsChanged
		}
	}
	for i, m := range current.Files {
		if m.Role != materials.Files[i].Role || !bytes.Equal(m.Raw, materials.Files[i].Raw) {
			return result, ErrMaterialsChanged
		}
	}
	if groupTree(request.DataOut, groupDataTree(false)) != nil || groupTree(request.SecretsOut, groupSecretTree(report.Component, false)) != nil {
		return result, ErrVerification
	}
	config, e := groupHash(ctx, filepath.Join(request.DataOut, "config.json"))
	if e != nil {
		return result, e
	}
	secret := secretManifest{Format: "dm-backup-secrets-v1", BackupID: id, Component: report.Component, SourceLayout: "flat-v1", Identity: report.Identity, DatabaseSHA256: after.sha, DatabaseBytes: after.bytes, Files: []secretFileDigest{}}
	for _, m := range materials.Files {
		stamp, e := groupHash(ctx, filepath.Join(request.SecretsOut, filepath.FromSlash(groupMaterialPath(m.Role))))
		if e != nil {
			return result, e
		}
		h := sha256.Sum256(m.Raw)
		if stamp.sha != base64.RawURLEncoding.EncodeToString(h[:]) || stamp.bytes != int64(len(m.Raw)) {
			return result, ErrVerification
		}
		file := secretFileDigest{Role: m.Role, Path: groupMaterialPath(m.Role), Bytes: stamp.bytes, SHA256: stamp.sha}
		if m.Role == RoleIdentity {
			secret.IdentitySHA256 = stamp.sha
		} else {
			for _, key := range report.Keys {
				if key.Role == m.Role {
					keyID := key.KeyID
					file.KeyID = &keyID
				}
			}
		}
		secret.Files = append(secret.Files, file)
	}
	rawSecret, e := groupJSON(secret)
	if e != nil {
		return result, e
	}
	if e = ops.syncDir(filepath.Join(request.SecretsOut, "keys")); e != nil {
		return result, ErrDestination
	}
	if e = groupPublish(ctx, result.SecretManifest, rawSecret, ops.rename); e != nil {
		return result, e
	}
	if e = ops.syncDir(request.SecretsOut); e != nil {
		return result, ErrDestination
	}
	external, _ := normalizeExternal(report.Component, request.Source.ExternalMaterials)
	digest := sha256.Sum256(rawSecret)
	manifest := dataManifest{Format: "dm-backup-v1", BackupID: id, Component: report.Component, SourceLayout: "flat-v1", Identity: report.Identity, SchemaVersion: 5, ProtocolVersion: 1, Creator: request.Source.Creator, SQLiteVersion: report.SQLiteVersion, StartedAt: started, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), Database: fileDigest{"sqlite/state.db", after.bytes, after.sha}, Files: []fileDigest{{"config.json", config.bytes, config.sha}}, IdentitySHA256: secret.IdentitySHA256, Keys: report.Keys, ReferencedEncryptionKeyIDs: report.ReferencedEncryptionKeyIDs, Inspection: inspectionDigest{report.SemanticFormat, report.SemanticFingerprint, report.TableRows, report.SealedRows}, SecretManifest: byteDigest{int64(len(rawSecret)), base64.RawURLEncoding.EncodeToString(digest[:])}, ExternalMaterials: external}
	if validateDataManifest(manifest) != nil {
		return result, ErrVerification
	}
	rawData, e := groupJSON(manifest)
	if e != nil {
		return result, e
	}
	// The last cancellable point is inside groupPublish, before no-replace rename.
	if e = groupPublish(ctx, result.DataManifest, rawData, ops.rename); e != nil {
		return result, e
	}
	result.Report = report
	result.State = StatePublicationUncertain
	if e = ops.syncDir(request.DataOut); e != nil {
		return result, ErrPublicationUncertain
	}
	result.State = StateComplete
	return result, nil
}
