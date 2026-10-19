//go:build linux

package degoo

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rclone/rclone/backend/degoo/auth"
	rfs "github.com/rclone/rclone/fs"
	"github.com/spf13/cobra"
)

const restoreCacheRemote = "degoo-live-test:"

func addRestoreCommands(root *cobra.Command) {
	var folderID string
	restore := &cobra.Command{
		Use:   "restore-cache original-vault.json new-vault.json",
		Short: "Rebind a copied offline cache to its new private vault configuration.",
		Long: `Rebinds the identity file in a copied vault cache after its cache directory
has moved. Both private vault configurations must use degoo-live-test:, and
--folder-id must match that configured remote's root_folder_id. This command is
offline: it reads local configuration and cache files without contacting Degoo.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return restoreCacheIdentity(args[0], args[1], folderID)
		},
	}
	restore.Flags().StringVar(&folderID, "folder-id", "", "Dedicated Degoo vault folder ID (required)")
	root.AddCommand(restore)
	var verifyFolderID string
	verify := &cobra.Command{
		Use:   "verify-cache vault.json",
		Short: "Verify the local cache identity without modifying or locking it.",
		Long: `Checks the private cache identity against the vault configuration and
the locally configured Degoo root folder. This command is offline and read-only;
maintenance callers must hold the cache lock while acting on its result.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return verifyCacheIdentity(args[0], verifyFolderID)
		},
	}
	verify.Flags().StringVar(&verifyFolderID, "folder-id", "", "Dedicated Degoo vault folder ID (required)")
	root.AddCommand(verify)
}

func verifyCacheIdentity(filename, folderID string) error {
	if strings.TrimSpace(folderID) != folderID || folderID == "" || folderID == "0" || strings.ContainsRune(folderID, '\x00') {
		return errors.New("--folder-id must identify a dedicated Degoo folder")
	}
	cfg, err := readConfig(filename)
	if err != nil {
		return fmt.Errorf("read vault configuration: %w", err)
	}
	if cfg.Remote != restoreCacheRemote {
		return fmt.Errorf("cache verification is restricted to %s", restoreCacheRemote)
	}
	if err := verifyRestoreFolderID(folderID); err != nil {
		return err
	}
	cacheInfo, err := os.Lstat(cfg.CacheDir)
	if err != nil {
		return err
	}
	if !cacheInfo.IsDir() || cacheInfo.Mode()&os.ModeSymlink != 0 || cacheInfo.Mode().Perm() != 0700 {
		return errors.New("cache directory must be a private directory with mode 0700")
	}
	expected, err := cacheIdentity(cfg, restoreCacheRemote+"\x00"+folderID)
	if err != nil {
		return err
	}
	current, err := auth.ReadPrivateFile(filepath.Join(cfg.CacheDir, cfg.cacheName()+".identity"))
	if err != nil {
		return fmt.Errorf("read cache identity: %w", err)
	}
	if !bytes.Equal(current, expected) {
		return errors.New("cache identity does not match the vault settings")
	}
	return nil
}

func restoreCacheIdentity(originalFilename, newFilename, folderID string) error {
	if strings.TrimSpace(folderID) != folderID || folderID == "" || folderID == "0" || strings.ContainsRune(folderID, '\x00') {
		return errors.New("--folder-id must identify a dedicated Degoo folder")
	}
	original, err := readConfig(originalFilename)
	if err != nil {
		return fmt.Errorf("read original vault configuration: %w", err)
	}
	newConfig, err := readConfig(newFilename)
	if err != nil {
		return fmt.Errorf("read new vault configuration: %w", err)
	}
	if original.Remote != restoreCacheRemote || newConfig.Remote != restoreCacheRemote {
		return fmt.Errorf("cache restore is restricted to %s", restoreCacheRemote)
	}
	if original.Version != newConfig.Version || original.Remote != newConfig.Remote || original.Password != newConfig.Password || original.PasswordSalt != newConfig.PasswordSalt || original.ChunkSize != newConfig.ChunkSize || original.MaxObjectSize != newConfig.MaxObjectSize {
		return errors.New("vault configurations differ beyond cache_dir; restore requires the original vault settings and password")
	}
	if err := verifyRestoreFolderID(folderID); err != nil {
		return err
	}

	backend := restoreCacheRemote + "\x00" + folderID
	originalIdentity, err := cacheIdentity(original, backend)
	if err != nil {
		return err
	}
	newIdentity, err := cacheIdentity(newConfig, backend)
	if err != nil {
		return err
	}

	cacheInfo, err := os.Lstat(newConfig.CacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("copied target cache directory does not exist")
		}
		return err
	}
	if !cacheInfo.IsDir() || cacheInfo.Mode()&os.ModeSymlink != 0 || cacheInfo.Mode().Perm() != 0700 {
		return errors.New("target cache directory must be a private directory with mode 0700")
	}
	lock, err := lockCache(newConfig)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	identityFilename := filepath.Join(newConfig.CacheDir, newConfig.cacheName()+".identity")
	current, err := auth.ReadPrivateFile(identityFilename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("copied cache identity is missing; refusing to rebind it")
		}
		return fmt.Errorf("read copied cache identity: %w", err)
	}
	if bytes.Equal(current, newIdentity) {
		return nil
	}
	if !bytes.Equal(current, originalIdentity) {
		return errors.New("copied cache identity does not match the original vault settings; refusing to rebind it")
	}
	if err := auth.WritePrivateFile(identityFilename, newIdentity); err != nil {
		return fmt.Errorf("write restored cache identity: %w", err)
	}
	return nil
}

func verifyRestoreFolderID(folderID string) error {
	info, configName, fsPath, remoteConfig, err := rfs.ConfigFs(restoreCacheRemote)
	if err != nil {
		return fmt.Errorf("read configured Degoo remote: %w", err)
	}
	if info.Name != "degoo" || configName != strings.TrimSuffix(restoreCacheRemote, ":") || fsPath != "" {
		return errors.New("configured restore remote is not the expected Degoo root")
	}
	configuredID, _ := remoteConfig.Get("root_folder_id")
	if configuredID == "" || configuredID == "0" || configuredID != folderID {
		return errors.New("--folder-id does not match the configured Degoo root_folder_id")
	}
	return nil
}
