//go:build linux

// Package degoo provides a private encrypted Degoo vault mount.
package degoo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rclone/rclone/backend/chunker"
	"github.com/rclone/rclone/backend/crypt"
	degoobackend "github.com/rclone/rclone/backend/degoo"
	"github.com/rclone/rclone/backend/degoo/auth"
	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/cmd/mountlib"
	rfs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/fspath"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/rclone/rclone/vfs/vfsflags"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// VaultConfig describes version 1 of the private local vault configuration.
type VaultConfig struct {
	// Version identifies the local configuration format.
	Version int `json:"version"`
	// Remote names the raw Degoo remote reserved for the vault.
	Remote string `json:"remote"`
	// Password is the independent vault encryption secret.
	Password string `json:"password"`
	// PasswordSalt supplies an optional independent crypt salt.
	PasswordSalt string `json:"password_salt,omitempty"`
	// CacheDir is the private persistent directory for pending local writes.
	CacheDir string `json:"cache_dir"`
	// ChunkSize is the maximum plaintext data size per encrypted chunk.
	ChunkSize rfs.SizeSuffix `json:"chunk_size"`
	// MaxObjectSize is the provisional encrypted-object safety cap.
	MaxObjectSize rfs.SizeSuffix `json:"max_object_size"`
}

type mountSettings struct {
	CacheMaxSize      *string `json:"cache_max_size,omitempty"`
	CacheMinFreeSpace *string `json:"cache_min_free_space,omitempty"`
	CacheMaxAge       *string `json:"cache_max_age,omitempty"`
	CachePollInterval *string `json:"cache_poll_interval,omitempty"`
	VerifyUpload      *bool   `json:"verify_upload,omitempty"`
}

func init() {
	root := &cobra.Command{Use: "degoo", Short: "Manage an encrypted Degoo vault on Linux."}
	cmd.Root.AddCommand(root)
	var email, restURL string
	var cachedEmail bool
	login := &cobra.Command{Use: "login auth.json", Short: "Log in without saving the account password.", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		if err := safeLogging(c.Context()); err != nil {
			return err
		}
		if strings.TrimSpace(email) == "" {
			return errors.New("--email is required")
		}
		email = strings.TrimSpace(email)
		password, err := promptPassword(loginPasswordPrompt(email, cachedEmail))
		if err != nil {
			return err
		}
		return auth.Login(c.Context(), fshttp.NewClient(c.Context()), restURL, args[0], email, password)
	}}
	login.Flags().StringVar(&email, "email", "", "Degoo account email")
	login.Flags().BoolVar(&cachedEmail, "cached-email", false, "Indicate that --email came from the wrapper's saved email")
	login.Flags().StringVar(&restURL, "rest-url", "https://rest-api.degoo.com", "Degoo login endpoint")
	logout := &cobra.Command{Use: "logout auth.json", Short: "Remove account tokens while keeping the vault secret and data.", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error { return auth.Logout(args[0]) }}
	var remote, cacheDir string
	initialize := &cobra.Command{Use: "init vault.json", Short: "Create a private vault configuration and prompt for its secret.", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		if _, err := os.Lstat(args[0]); !os.IsNotExist(err) {
			return errors.New("vault configuration already exists or is inaccessible")
		}
		password, err := promptPassword("New vault password: ")
		if err != nil {
			return err
		}
		confirmation, err := promptPassword("Repeat vault password: ")
		if err != nil {
			return err
		}
		if password != confirmation {
			return errors.New("vault passwords differ")
		}
		cfg := VaultConfig{Version: 1, Remote: remote, Password: password, CacheDir: cacheDir}
		if err := cfg.validate(); err != nil {
			return err
		}
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		return auth.WritePrivateFile(args[0], append(data, '\n'))
	}}
	initialize.Flags().StringVar(&remote, "remote", "", "Configured raw Degoo remote reserved for this vault (required)")
	initialize.Flags().StringVar(&cacheDir, "cache-dir", "", "Private persistent cache directory (required)")
	var settingsFile string
	var verifyUpload bool
	mount := &cobra.Command{Use: "mount vault.json /path/to/mountpoint", Short: "Mount a writable vault with encrypted names, contents, and chunk metadata.", Long: `Mount a private Degoo vault through VFS, chunker, and crypt.
The configuration must be a private version 1 JSON file. Encryption and full
VFS caching are enforced. fsync commits to the local cache; uploads finish
asynchronously. Keep the cache until pending uploads complete. Use degoo status
to count locally pending files. Only one process may use a vault cache at a time.
Live Degoo behavior is experimental; use a disposable vault first. Upload readback
verification is enabled by default. The private --settings file may set cache
limits and verify_upload; explicit command-line flags take precedence.`, Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, args []string) error {
		return mountVault(c.Context(), args[0], args[1], settingsFile, c.Flags(), verifyUpload)
	}}
	mountlib.AddFlags(mount.Flags())
	vfsflags.AddFlags(mount.Flags())
	mount.Flags().StringVar(&settingsFile, "settings", "", "Private JSON file with optional mount settings")
	mount.Flags().BoolVar(&verifyUpload, "verify-upload", true, "Read back and hash each uploaded object before publishing it")
	status := &cobra.Command{Use: "status vault.json", Short: "Show the number and size of locally pending uploads.", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		cfg, err := readConfig(args[0])
		if err != nil {
			return err
		}
		files, size, err := pending(cfg)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(c.OutOrStdout(), "Pending local uploads: %d files, %d bytes\n", files, size)
		return err
	}}
	root.AddCommand(initialize, login, logout, mount, status)
	addSetupCommands(root)
	addTrashCommands(root)
}

func promptPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("password entry requires an interactive terminal")
	}
	_, _ = fmt.Fprint(config.PasswordPromptOutput, prompt)
	password, err := term.ReadPassword(fd)
	_, _ = fmt.Fprintln(config.PasswordPromptOutput)
	if err != nil {
		return "", errors.New("could not read password")
	}
	return string(password), nil
}

func loginPasswordPrompt(email string, cachedEmail bool) string {
	if cachedEmail {
		return fmt.Sprintf("Degoo account password for %s (cached email; clear with ~/.local/share/rclone-degoo/login.sh --clear-email): ", email)
	}
	return fmt.Sprintf("Degoo account password for %s: ", email)
}

func safeLogging(ctx context.Context) error {
	if rfs.GetConfig(ctx).Dump != 0 {
		return errors.New("HTTP dumps are disabled for Degoo because requests contain account tokens")
	}
	return nil
}

func readConfig(filename string) (*VaultConfig, error) {
	data, err := auth.ReadPrivateFile(filename)
	if err != nil {
		return nil, err
	}
	var cfg VaultConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, errors.New("invalid vault configuration JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("vault configuration contains trailing data")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func readMountSettings(filename string) (*mountSettings, error) {
	if filename == "" {
		return &mountSettings{}, nil
	}
	data, err := auth.ReadPrivateFile(filename)
	if err != nil {
		return nil, err
	}
	var settings mountSettings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return nil, errors.New("invalid mount settings JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("mount settings contain trailing data")
	}
	return &settings, nil
}

func (s *mountSettings) apply(options *vfscommon.Options, flags *pflag.FlagSet, verifyUploadFlag bool) (bool, error) {
	var cacheMaxSize rfs.SizeSuffix
	if s.CacheMaxSize != nil {
		if err := cacheMaxSize.Set(*s.CacheMaxSize); err != nil {
			return false, fmt.Errorf("invalid cache_max_size: %w", err)
		}
	}
	var cacheMinFreeSpace rfs.SizeSuffix
	if s.CacheMinFreeSpace != nil {
		if err := cacheMinFreeSpace.Set(*s.CacheMinFreeSpace); err != nil {
			return false, fmt.Errorf("invalid cache_min_free_space: %w", err)
		}
	}
	var cacheMaxAge rfs.Duration
	if s.CacheMaxAge != nil {
		if err := cacheMaxAge.Set(*s.CacheMaxAge); err != nil {
			return false, fmt.Errorf("invalid cache_max_age: %w", err)
		}
	}
	var cachePollInterval rfs.Duration
	if s.CachePollInterval != nil {
		if err := cachePollInterval.Set(*s.CachePollInterval); err != nil {
			return false, fmt.Errorf("invalid cache_poll_interval: %w", err)
		}
	}
	if s.CacheMaxSize != nil && !flagChanged(flags, "vfs-cache-max-size") {
		options.CacheMaxSize = cacheMaxSize
	}
	if s.CacheMinFreeSpace != nil && !flagChanged(flags, "vfs-cache-min-free-space") {
		options.CacheMinFreeSpace = cacheMinFreeSpace
	}
	if s.CacheMaxAge != nil && !flagChanged(flags, "vfs-cache-max-age") {
		options.CacheMaxAge = cacheMaxAge
	}
	if s.CachePollInterval != nil && !flagChanged(flags, "vfs-cache-poll-interval") {
		options.CachePollInterval = cachePollInterval
	}
	verifyUpload := true
	if s.VerifyUpload != nil {
		verifyUpload = *s.VerifyUpload
	}
	if flagChanged(flags, "verify-upload") {
		verifyUpload = verifyUploadFlag
	}
	return verifyUpload, nil
}

func flagChanged(flags *pflag.FlagSet, name string) bool {
	return flags != nil && flags.Changed(name)
}

func mountRawConfig(remote string, verify bool) (*rfs.RegInfo, string, string, configmap.Mapper, error) {
	fsInfo, configName, fsPath, originalConfig, err := rfs.ConfigFs(remote)
	if err != nil {
		return nil, "", "", nil, err
	}
	if fsInfo.Name != "degoo" {
		return nil, "", "", nil, errors.New("mount remote must be a configured Degoo remote")
	}
	config := configmap.New().
		AddGetter(configmap.Simple{"skip_upload_verification": strconv.FormatBool(!verify)}, configmap.PriorityNormal).
		AddGetter(originalConfig, configmap.PriorityMax)
	return fsInfo, configName, fsPath, config, nil
}

func newMountRawFs(ctx context.Context, remote string, verify bool) (rfs.Fs, error) {
	fsInfo, configName, fsPath, config, err := mountRawConfig(remote, verify)
	if err != nil {
		return nil, err
	}
	return fsInfo.NewFs(ctx, configName, fsPath, config)
}

func (c *VaultConfig) validate() error {
	if c.Version != 1 {
		return errors.New("vault configuration version must be 1")
	}
	if c.Password == "" {
		return errors.New("vault password is required; plaintext mounts are unavailable")
	}
	name, _, found := strings.Cut(c.Remote, ":")
	if !found || fspath.CheckConfigName(name) != nil {
		return errors.New("remote must name a configured Degoo remote")
	}
	if !filepath.IsAbs(c.CacheDir) {
		return errors.New("cache_dir must be an absolute persistent directory")
	}
	if c.ChunkSize == 0 {
		c.ChunkSize = rfs.Gibi
	}
	if c.MaxObjectSize == 0 {
		c.MaxObjectSize = 19 * rfs.Gibi
	}
	if c.ChunkSize < 1 || c.MaxObjectSize < 1 || c.MaxObjectSize > 19*rfs.Gibi {
		return errors.New("chunk and object sizes must be positive; max_object_size must not exceed the provisional 19 GiB safety cap")
	}
	cipher, err := crypt.NewCipher(configmap.Simple{"password": obscure.MustObscure(c.Password), "filename_encryption": "standard", "filename_encoding": "base32", "directory_name_encryption": "true", "suffix": ".bin"})
	if err != nil {
		return errors.New("invalid vault cipher configuration")
	}
	if c.ChunkSize > c.MaxObjectSize || cipher.EncryptedSize(int64(c.ChunkSize)) > int64(c.MaxObjectSize) || cipher.EncryptedSize(255) > int64(c.MaxObjectSize) {
		return errors.New("encrypted chunk size exceeds max_object_size")
	}
	return nil
}

func (c *VaultConfig) cacheName() string {
	sum := sha256.Sum256([]byte(c.Remote))
	return "degoo-vault-" + hex.EncodeToString(sum[:12])
}

// newStack constructs both overlays with fixed encryption and manifest settings.
func newStack(ctx context.Context, base rfs.Fs, c *VaultConfig) (rfs.Fs, error) {
	name := c.cacheName()
	cache.Put(name+"-raw:", base)
	password, err := obscure.Obscure(c.Password)
	if err != nil {
		return nil, errors.New("could not prepare vault cipher")
	}
	salt, err := obscure.Obscure(c.PasswordSalt)
	if err != nil {
		return nil, errors.New("could not prepare vault salt")
	}
	cryptConfig := configmap.Simple{"remote": name + "-raw:", "password": password, "password2": salt, "filename_encryption": "standard", "filename_encoding": "base32", "directory_name_encryption": "true", "no_data_encryption": "false", "show_mapping": "false", "strict_names": "true", "suffix": ".bin"}
	cf, err := crypt.NewFs(ctx, name+"-crypt", "", cryptConfig)
	if err != nil {
		return nil, err
	}
	cache.Put(name+"-crypt:", cf)
	chunkConfig := configmap.Simple{"remote": name + "-crypt:", "chunk_size": fmt.Sprintf("%dB", int64(c.ChunkSize)), "name_format": "*.rclone_chunk.###", "start_from": "1", "meta_format": "simplejson", "hash_type": "sha1all", "transactions": "norename", "fail_hard": "true"}
	return chunker.NewFs(ctx, name, "", chunkConfig)
}

func privateCache(dirname string) error {
	if err := os.MkdirAll(dirname, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dirname)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return errors.New("cache directory must be a private directory with mode 0700")
	}
	return nil
}

func lockCache(c *VaultConfig) (*os.File, error) {
	if err := privateCache(c.CacheDir); err != nil {
		return nil, err
	}
	filename := filepath.Join(c.CacheDir, c.cacheName()+".lock")
	fd, err := unix.Open(filename, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), filename)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("vault cache is already locked by another process")
	}
	return f, nil
}

func bindCache(c *VaultConfig, base rfs.Fs) error {
	backend := rfs.ConfigString(base)
	if df, ok := base.(*degoobackend.Fs); ok {
		backend += "\x00" + df.RootFolderID()
	}
	identity, err := json.Marshal(struct {
		Config  *VaultConfig
		Backend string
	}{c, backend})
	if err != nil {
		return err
	}
	sum := sha256.Sum256(identity)
	data := []byte(hex.EncodeToString(sum[:]) + "\n")
	filename := filepath.Join(c.CacheDir, c.cacheName()+".identity")
	old, err := auth.ReadPrivateFile(filename)
	if os.IsNotExist(err) {
		for _, subdir := range []string{"vfs", "vfsMeta"} {
			entries, readErr := os.ReadDir(filepath.Join(c.CacheDir, subdir, c.cacheName()))
			if readErr != nil && !os.IsNotExist(readErr) {
				return readErr
			}
			if len(entries) != 0 {
				return errors.New("vault cache identity is missing but cached data exists; restore the original identity before mounting")
			}
		}
		return auth.WritePrivateFile(filename, data)
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(old, data) {
		return errors.New("vault settings differ from this cache; restore the original settings to recover pending writes")
	}
	return nil
}

func mountVault(ctx context.Context, filename, mountpoint, settingsFile string, flags *pflag.FlagSet, verifyUploadFlag bool) error {
	if err := safeLogging(ctx); err != nil {
		return err
	}
	if mountlib.Opt.Daemon {
		return errors.New("degoo mount runs in the foreground; use a service manager for background operation")
	}
	c, err := readConfig(filename)
	if err != nil {
		return err
	}
	settings, err := readMountSettings(settingsFile)
	if err != nil {
		return err
	}
	options := vfscommon.Opt
	verifyUpload, err := settings.apply(&options, flags, verifyUploadFlag)
	if err != nil {
		return err
	}
	lock, err := lockCache(c)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	base, err := cache.Get(ctx, c.Remote)
	if err != nil {
		return err
	}
	df, ok := base.(*degoobackend.Fs)
	if !ok || df.RootFolderID() == "" || df.RootFolderID() == "0" {
		return errors.New("vault remote must be a Degoo backend rooted in a dedicated non-account folder")
	}
	if err := bindCache(c, base); err != nil {
		return err
	}
	// Bind the cache to the configured remote above. The verification setting is
	// scoped to the mounted raw filesystem and must not change the cache identity.
	mountBase, err := newMountRawFs(ctx, c.Remote, verifyUpload)
	if err != nil {
		return err
	}
	mountDF, ok := mountBase.(*degoobackend.Fs)
	if !ok || mountDF.RootFolderID() != df.RootFolderID() {
		return errors.New("verification override did not preserve the configured Degoo vault remote")
	}
	stack, err := newStack(ctx, mountBase, c)
	if err != nil {
		return err
	}
	if err := config.SetCacheDir(c.CacheDir); err != nil {
		return err
	}
	_, mountFn := mountlib.ResolveMountMethod("")
	if mountFn == nil {
		return errors.New("this build has no FUSE mount method")
	}
	guardedMount := func(v *vfs.VFS, mountpoint string, opt *mountlib.Options) (<-chan error, func() error, string, error) {
		if v.Opt.CacheMode != vfscommon.CacheModeFull || v.Stats()["diskCache"] == nil {
			return nil, nil, "", errors.New("durable VFS cache unavailable; refusing to mount")
		}
		return mountFn(v, mountpoint, opt)
	}
	options.CacheMode = vfscommon.CacheModeFull
	options.PollInterval = 0
	mnt := mountlib.NewMountPoint(guardedMount, mountpoint, stack, &mountlib.Opt, &options)
	mnt.Ctx = ctx
	rfs.Logf(nil, "Vault writes are saved in the local cache; remote uploads complete asynchronously. Keep the cache for recovery.")
	_, err = mnt.Mount()
	if err != nil {
		return err
	}
	return mnt.Wait()
}

func pending(c *VaultConfig) (files, size int64, err error) {
	dirname := filepath.Join(c.CacheDir, "vfsMeta", c.cacheName())
	err = filepath.WalkDir(dirname, func(filename string, entry fs.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) && filename == dirname {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("unexpected non-regular cache metadata")
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		var metadata struct {
			Dirty bool
			Size  int64
		}
		if err := json.Unmarshal(data, &metadata); err != nil {
			return errors.New("could not read cache metadata; retry after current writes finish")
		}
		if metadata.Dirty {
			files++
			size += metadata.Size
		}
		return nil
	})
	return
}
