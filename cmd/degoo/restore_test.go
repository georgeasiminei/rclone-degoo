//go:build linux

package degoo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rclone/rclone/backend/degoo/auth"
	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

const restoreTestFolderID = "dedicated-test-folder"

func setRestoreTestRemote(t *testing.T, folderID string) {
	t.Helper()
	typeEnv := fs.ConfigToEnv("degoo-live-test", "type")
	rootEnv := fs.ConfigToEnv("degoo-live-test", "root_folder_id")
	oldType, hadType := os.LookupEnv(typeEnv)
	oldRoot, hadRoot := os.LookupEnv(rootEnv)
	require.NoError(t, os.Setenv(typeEnv, "degoo"))
	require.NoError(t, os.Setenv(rootEnv, folderID))
	t.Cleanup(func() {
		if hadType {
			_ = os.Setenv(typeEnv, oldType)
		} else {
			_ = os.Unsetenv(typeEnv)
		}
		if hadRoot {
			_ = os.Setenv(rootEnv, oldRoot)
		} else {
			_ = os.Unsetenv(rootEnv)
		}
	})
}

func restoreTestConfigs(t *testing.T) (*VaultConfig, *VaultConfig, string, string) {
	t.Helper()
	original := testConfig(t)
	original.Remote = restoreCacheRemote
	newConfig := *original
	newConfig.CacheDir = filepath.Join(t.TempDir(), "cache")
	require.NoError(t, os.Mkdir(newConfig.CacheDir, 0700))
	originalFilename := filepath.Join(t.TempDir(), "private", "original-vault.json")
	newFilename := filepath.Join(t.TempDir(), "private", "new-vault.json")
	writeVault := func(filename string, cfg *VaultConfig) {
		data, err := json.Marshal(cfg)
		require.NoError(t, err)
		require.NoError(t, auth.WritePrivateFile(filename, data))
	}
	writeVault(originalFilename, original)
	writeVault(newFilename, &newConfig)
	return original, &newConfig, originalFilename, newFilename
}

func writeRestoreIdentity(t *testing.T, cfg *VaultConfig, data []byte) string {
	t.Helper()
	filename := filepath.Join(cfg.CacheDir, cfg.cacheName()+".identity")
	require.NoError(t, auth.WritePrivateFile(filename, data))
	return filename
}

func TestRestoreCacheIdentityRebindsOnlyIdentityAndIsIdempotent(t *testing.T) {
	setRestoreTestRemote(t, restoreTestFolderID)
	original, newConfig, originalFilename, newFilename := restoreTestConfigs(t)
	backend := restoreCacheRemote + "\x00" + restoreTestFolderID
	oldIdentity, err := cacheIdentity(original, backend)
	require.NoError(t, err)
	newIdentity, err := cacheIdentity(newConfig, backend)
	require.NoError(t, err)
	identityFilename := writeRestoreIdentity(t, newConfig, oldIdentity)
	metadataDir := filepath.Join(newConfig.CacheDir, "vfsMeta", newConfig.cacheName())
	require.NoError(t, os.MkdirAll(metadataDir, 0700))
	metadataFilename := filepath.Join(metadataDir, "pending")
	require.NoError(t, os.WriteFile(metadataFilename, []byte(`{"Dirty":true,"Size":8}`), 0600))
	sourceIdentityFilename := writeRestoreIdentity(t, original, oldIdentity)

	require.NoError(t, restoreCacheIdentity(originalFilename, newFilename, restoreTestFolderID))
	gotIdentity, err := auth.ReadPrivateFile(identityFilename)
	require.NoError(t, err)
	require.Equal(t, newIdentity, gotIdentity)
	gotMetadata, err := os.ReadFile(metadataFilename)
	require.NoError(t, err)
	require.Equal(t, []byte(`{"Dirty":true,"Size":8}`), gotMetadata)
	gotSourceIdentity, err := auth.ReadPrivateFile(sourceIdentityFilename)
	require.NoError(t, err)
	require.Equal(t, oldIdentity, gotSourceIdentity)

	require.NoError(t, restoreCacheIdentity(originalFilename, newFilename, restoreTestFolderID))
	gotIdentity, err = auth.ReadPrivateFile(identityFilename)
	require.NoError(t, err)
	require.Equal(t, newIdentity, gotIdentity)
	gotMetadata, err = os.ReadFile(metadataFilename)
	require.NoError(t, err)
	require.Equal(t, []byte(`{"Dirty":true,"Size":8}`), gotMetadata)
}

func TestRestoreCacheIdentityRejectsWrongSecretFolderAndIdentity(t *testing.T) {
	setRestoreTestRemote(t, restoreTestFolderID)
	t.Run("wrong secret", func(t *testing.T) {
		original, newConfig, originalFilename, newFilename := restoreTestConfigs(t)
		identity, err := cacheIdentity(original, restoreCacheRemote+"\x00"+restoreTestFolderID)
		require.NoError(t, err)
		identityFilename := writeRestoreIdentity(t, newConfig, identity)
		newConfig.Password = "different vault password"
		data, err := json.Marshal(newConfig)
		require.NoError(t, err)
		require.NoError(t, auth.WritePrivateFile(newFilename, data))

		require.Error(t, restoreCacheIdentity(originalFilename, newFilename, restoreTestFolderID))
		got, err := auth.ReadPrivateFile(identityFilename)
		require.NoError(t, err)
		require.Equal(t, identity, got)
	})
	t.Run("wrong configured folder", func(t *testing.T) {
		original, newConfig, originalFilename, newFilename := restoreTestConfigs(t)
		identity, err := cacheIdentity(original, restoreCacheRemote+"\x00"+restoreTestFolderID)
		require.NoError(t, err)
		identityFilename := writeRestoreIdentity(t, newConfig, identity)

		require.Error(t, restoreCacheIdentity(originalFilename, newFilename, "another-folder"))
		got, err := auth.ReadPrivateFile(identityFilename)
		require.NoError(t, err)
		require.Equal(t, identity, got)
	})
	t.Run("mismatched identity", func(t *testing.T) {
		_, newConfig, originalFilename, newFilename := restoreTestConfigs(t)
		identityFilename := writeRestoreIdentity(t, newConfig, []byte("corrupt identity\n"))

		require.Error(t, restoreCacheIdentity(originalFilename, newFilename, restoreTestFolderID))
		got, err := auth.ReadPrivateFile(identityFilename)
		require.NoError(t, err)
		require.Equal(t, []byte("corrupt identity\n"), got)
	})
	t.Run("missing identity", func(t *testing.T) {
		_, newConfig, originalFilename, newFilename := restoreTestConfigs(t)

		require.Error(t, restoreCacheIdentity(originalFilename, newFilename, restoreTestFolderID))
		_, err := os.Lstat(filepath.Join(newConfig.CacheDir, newConfig.cacheName()+".identity"))
		require.True(t, os.IsNotExist(err))
	})
}

func TestRestoreCacheIdentityRejectsActiveCacheLock(t *testing.T) {
	setRestoreTestRemote(t, restoreTestFolderID)
	original, newConfig, originalFilename, newFilename := restoreTestConfigs(t)
	identity, err := cacheIdentity(original, restoreCacheRemote+"\x00"+restoreTestFolderID)
	require.NoError(t, err)
	identityFilename := writeRestoreIdentity(t, newConfig, identity)
	lock, err := lockCache(newConfig)
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Close()) }()

	require.Error(t, restoreCacheIdentity(originalFilename, newFilename, restoreTestFolderID))
	got, err := auth.ReadPrivateFile(identityFilename)
	require.NoError(t, err)
	require.Equal(t, identity, got)
}

func TestVerifyCacheIdentityIsReadOnlyWithActiveLock(t *testing.T) {
	setRestoreTestRemote(t, restoreTestFolderID)
	_, cfg, _, filename := restoreTestConfigs(t)
	identity, err := cacheIdentity(cfg, restoreCacheRemote+"\x00"+restoreTestFolderID)
	require.NoError(t, err)
	identityFilename := writeRestoreIdentity(t, cfg, identity)
	before, err := os.Stat(identityFilename)
	require.NoError(t, err)
	lock, err := lockCache(cfg)
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Close()) }()

	require.NoError(t, verifyCacheIdentity(filename, restoreTestFolderID))
	require.Error(t, verifyCacheIdentity(filename, "another-folder"))
	after, err := os.Stat(identityFilename)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.Equal(t, before.ModTime(), after.ModTime())
	got, err := auth.ReadPrivateFile(identityFilename)
	require.NoError(t, err)
	require.Equal(t, identity, got)
}

func TestVerifyCacheIdentityRejectsMissingAndMismatchedIdentity(t *testing.T) {
	setRestoreTestRemote(t, restoreTestFolderID)
	_, cfg, _, filename := restoreTestConfigs(t)
	require.Error(t, verifyCacheIdentity(filename, restoreTestFolderID))
	entries, err := os.ReadDir(cfg.CacheDir)
	require.NoError(t, err)
	require.Empty(t, entries)
	identityFilename := writeRestoreIdentity(t, cfg, []byte("different identity\n"))
	require.Error(t, verifyCacheIdentity(filename, restoreTestFolderID))
	got, err := auth.ReadPrivateFile(identityFilename)
	require.NoError(t, err)
	require.Equal(t, []byte("different identity\n"), got)
}
