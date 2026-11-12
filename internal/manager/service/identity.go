package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"unicode/utf8"

	"docker-manager/internal/manager/operations"
	"docker-manager/internal/protocol"
)

type IdentityManifest struct {
	FormatVersion   int    `json:"format_version"`
	ManagerID       string `json:"manager_id"`
	SchemaVersion   int    `json:"schema_version"`
	DigestKeyID     string `json:"digest_key_id"`
	EncryptionKeyID string `json:"encryption_key_id"`
}

func (i IdentityManifest) StoreIdentity() operations.Identity {
	return operations.Identity{ManagerID: i.ManagerID, DigestKeyID: i.DigestKeyID, EncryptionKeyID: i.EncryptionKeyID}
}

func clearKeys(k *operations.Keys) { clear(k.DigestKey); clear(k.EncryptionKey) }

var bootstrapName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

func validateRuntimeConfig(c Config) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return ErrConfig
	}
	_, err = ParseConfig(raw)
	return err
}

func Initialize(ctx context.Context, c Config, username, password string) (IdentityManifest, error) {
	if err := validateRuntimeConfig(c); err != nil {
		return IdentityManifest{}, err
	}
	if !bootstrapName.MatchString(username) || !utf8.ValidString(password) || utf8.RuneCountInString(password) < 12 || utf8.RuneCountInString(password) > 128 {
		return IdentityManifest{}, ErrConfig
	}
	if err := ctx.Err(); err != nil {
		return IdentityManifest{}, err
	}
	dataDir := filepath.Dir(c.DatabaseFile)
	if err := ensurePrivateDir(dataDir); err != nil {
		return IdentityManifest{}, err
	}
	if err := ensurePrivateDir(c.KeysDir); err != nil {
		return IdentityManifest{}, err
	}
	lock, err := acquireFileLock(filepath.Join(dataDir, ".manager.lock"))
	if err != nil {
		return IdentityManifest{}, err
	}
	defer lock.Close()
	paths := []string{c.DatabaseFile, filepath.Join(dataDir, "identity.json"), filepath.Join(c.KeysDir, "digest.key"), filepath.Join(c.KeysDir, "encryption.key")}
	unique := map[string]bool{}
	for _, path := range paths {
		if err := rejectLinks(path); err != nil {
			return IdentityManifest{}, err
		}
		if unique[filepath.Clean(path)] {
			return IdentityManifest{}, ErrConfig
		}
		unique[filepath.Clean(path)] = true
		if _, err := os.Lstat(path); err == nil {
			return IdentityManifest{}, ErrInitialized
		} else if !os.IsNotExist(err) {
			return IdentityManifest{}, ErrFiles
		}
	}
	manager, err := protocol.NewID()
	if err != nil {
		return IdentityManifest{}, ErrFiles
	}
	digestID, err := protocol.NewID()
	if err != nil {
		return IdentityManifest{}, ErrFiles
	}
	encryptionID, err := protocol.NewID()
	if err != nil {
		return IdentityManifest{}, ErrFiles
	}
	identity := IdentityManifest{1, manager, 4, digestID, encryptionID}
	keys := operations.Keys{DigestID: digestID, EncryptionID: encryptionID, DigestKey: make([]byte, 32), EncryptionKey: make([]byte, 32)}
	defer clearKeys(&keys)
	if _, err := rand.Read(keys.DigestKey); err != nil {
		return IdentityManifest{}, ErrFiles
	}
	if _, err := rand.Read(keys.EncryptionKey); err != nil {
		return IdentityManifest{}, ErrFiles
	}
	if err := writeExclusive(paths[2], keys.DigestKey); err != nil {
		return IdentityManifest{}, err
	}
	if err := writeExclusive(paths[3], keys.EncryptionKey); err != nil {
		return IdentityManifest{}, err
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return IdentityManifest{}, ErrFiles
	}
	if err := writeExclusive(paths[1], raw); err != nil {
		return IdentityManifest{}, err
	}
	store, err := operations.CreateService(ctx, c.DatabaseFile, manager, keys, operations.Bootstrap{Username: username, Password: password})
	if err != nil {
		return IdentityManifest{}, err
	}
	if err := store.Close(); err != nil {
		return IdentityManifest{}, err
	}
	if err := syncDirectory(dataDir); err != nil {
		return IdentityManifest{}, err
	}
	return identity, nil
}

func loadIdentity(c Config) (IdentityManifest, operations.Keys, error) {
	var identity IdentityManifest
	raw, err := readPrivateFile(filepath.Join(filepath.Dir(c.DatabaseFile), "identity.json"), 4096)
	if err != nil {
		return identity, operations.Keys{}, err
	}
	if _, err := protocol.StrictJSON(raw); err != nil {
		return identity, operations.Keys{}, ErrFiles
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&identity); err != nil || identity.FormatVersion != 1 || identity.SchemaVersion != 4 {
		return IdentityManifest{}, operations.Keys{}, ErrFiles
	}
	// Store verifies UUIDs, key identifiers and their checks against the DB.
	if identity.ManagerID == "" || identity.DigestKeyID == "" || identity.EncryptionKeyID == "" || identity.DigestKeyID == identity.EncryptionKeyID {
		return IdentityManifest{}, operations.Keys{}, ErrFiles
	}
	digest, err := readPrivateFile(filepath.Join(c.KeysDir, "digest.key"), 32)
	if err != nil {
		return IdentityManifest{}, operations.Keys{}, err
	}
	encryption, err := readPrivateFile(filepath.Join(c.KeysDir, "encryption.key"), 32)
	if err != nil || len(digest) != 32 || len(encryption) != 32 {
		clear(digest)
		clear(encryption)
		return IdentityManifest{}, operations.Keys{}, ErrFiles
	}
	return identity, operations.Keys{DigestID: identity.DigestKeyID, EncryptionID: identity.EncryptionKeyID, DigestKey: digest, EncryptionKey: encryption}, nil
}
