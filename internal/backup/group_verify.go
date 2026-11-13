package backup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"path/filepath"
	"reflect"
)

// VerifyGroup reads only the explicitly selected closed backup group. It never
// creates a connection to the live source or changes any backup artifact.
func VerifyGroup(ctx context.Context, dataManifestFile, secretManifestFile string, inspect DomainInspector) (result GroupResult, err error) {
	if ctx == nil || inspect == nil {
		return result, ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	if e := ctx.Err(); e != nil {
		return result, e
	}
	if filepath.Base(dataManifestFile) != "manifest.json" || filepath.Base(secretManifestFile) != "secret-manifest.json" {
		return result, ErrVerification
	}
	fail := func() (GroupResult, error) { return GroupResult{}, ErrVerification }
	raw, e := groupRead(dataManifestFile, groupJSONLimit)
	if e != nil {
		return fail()
	}
	var manifest dataManifest
	if strictGroupJSON(raw, &manifest) != nil || validateDataManifest(manifest) != nil {
		return fail()
	}
	dataRoot, secretRoot := filepath.Dir(dataManifestFile), filepath.Dir(secretManifestFile)
	if groupTree(dataRoot, groupDataTree(true)) != nil {
		return fail()
	}
	database := filepath.Join(dataRoot, "sqlite", "state.db")
	before, e := groupClosedHash(ctx, database)
	if e != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return fail()
	}
	if before.bytes != manifest.Database.Bytes || before.sha != manifest.Database.SHA256 {
		return fail()
	}
	configRaw, e := groupRead(filepath.Join(dataRoot, "config.json"), groupJSONLimit)
	if e != nil || validateGroupConfig(manifest.Component, configRaw) != nil {
		return fail()
	}
	hash := sha256.Sum256(configRaw)
	if int64(len(configRaw)) != manifest.Files[0].Bytes || base64.RawURLEncoding.EncodeToString(hash[:]) != manifest.Files[0].SHA256 {
		return fail()
	}
	secretRaw, e := groupRead(secretManifestFile, groupJSONLimit)
	if e != nil {
		return fail()
	}
	hash = sha256.Sum256(secretRaw)
	if int64(len(secretRaw)) != manifest.SecretManifest.Bytes || base64.RawURLEncoding.EncodeToString(hash[:]) != manifest.SecretManifest.SHA256 {
		return fail()
	}
	var secret secretManifest
	if strictGroupJSON(secretRaw, &secret) != nil || secret.Format != "dm-backup-secrets-v1" || secret.BackupID != manifest.BackupID || secret.Component != manifest.Component || secret.SourceLayout != manifest.SourceLayout || secret.Generation != nil || secret.Identity != manifest.Identity || secret.DatabaseSHA256 != manifest.Database.SHA256 || secret.DatabaseBytes != manifest.Database.Bytes || secret.IdentitySHA256 != manifest.IdentitySHA256 || len(secret.Files) != 3 {
		return fail()
	}
	if groupTree(secretRoot, groupSecretTree(manifest.Component, true)) != nil {
		return fail()
	}
	materials := MaterialSet{ConfigJSON: configRaw}
	defer groupClear(&materials)
	for i, role := range groupRoles(manifest.Component) {
		file := secret.Files[i]
		if file.Role != role || file.Path != groupMaterialPath(role) || !groupToken32(file.SHA256) {
			return fail()
		}
		limit := int64(32)
		if role == RoleIdentity {
			limit = groupIdentityLimit
			if file.KeyID != nil || file.SHA256 != manifest.IdentitySHA256 {
				return fail()
			}
		} else {
			var found bool
			for _, key := range manifest.Keys {
				if key.Role == role && file.KeyID != nil && *file.KeyID == key.KeyID {
					found = true
				}
			}
			if !found || file.Bytes != 32 {
				return fail()
			}
		}
		raw, e := groupRead(filepath.Join(secretRoot, filepath.FromSlash(file.Path)), limit)
		if e != nil || len(raw) == 0 {
			return fail()
		}
		materials.Files = append(materials.Files, Material{Role: role, Raw: raw})
		hash = sha256.Sum256(raw)
		if file.Bytes != int64(len(raw)) || file.SHA256 != base64.RawURLEncoding.EncodeToString(hash[:]) {
			return fail()
		}
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
		return fail()
	}
	report, e = normalizeReport(report)
	if e != nil {
		return fail()
	}
	expected := manifestReport(manifest)
	expected.SQLiteVersion = report.SQLiteVersion
	if !reflect.DeepEqual(report, expected) {
		return fail()
	}
	if e = ctx.Err(); e != nil {
		return result, e
	}
	return GroupResult{BackupID: manifest.BackupID, DataManifest: dataManifestFile, SecretManifest: secretManifestFile, State: StateComplete, Report: report}, nil
}
