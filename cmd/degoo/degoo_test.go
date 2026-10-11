//go:build linux

package degoo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/auth"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func testConfig(t *testing.T) *VaultConfig {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	require.NoError(t, os.Mkdir(cacheDir, 0700))
	return &VaultConfig{Version: 1, Remote: "TestDegoo-" + strings.ReplaceAll(t.Name(), "/", "-") + ":", Password: "generated vault password", CacheDir: cacheDir, ChunkSize: 1024, MaxObjectSize: 4096}
}

func localStack(t *testing.T, cfg *VaultConfig) (fs.Fs, fs.Fs) {
	t.Helper()
	base, err := fs.NewFs(context.Background(), t.TempDir())
	require.NoError(t, err)
	stack, err := newStack(context.Background(), base, cfg)
	require.NoError(t, err)
	return base, stack
}

func put(t *testing.T, f fs.Fs, name string, data []byte) fs.Object {
	t.Helper()
	info := object.NewStaticObjectInfo(name, time.Now(), int64(len(data)), true, nil, f)
	obj, err := f.Put(context.Background(), bytes.NewReader(data), info)
	require.NoError(t, err)
	return obj
}

func read(t *testing.T, obj fs.Object, options ...fs.OpenOption) []byte {
	t.Helper()
	r, err := obj.Open(context.Background(), options...)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	return got
}

func TestVaultConfigurationAndCacheProtection(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	require.NoError(t, cfg.validate())
	filename := filepath.Join(t.TempDir(), "private", "vault.json")
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, auth.WritePrivateFile(filename, data))
	got, err := readConfig(filename)
	require.NoError(t, err)
	require.Equal(t, cfg, got)
	require.NoError(t, os.Chmod(filename, 0644))
	_, err = readConfig(filename)
	require.Error(t, err)
	bad := *cfg
	bad.Password = ""
	require.Error(t, bad.validate())
	bad = *cfg
	bad.ChunkSize = bad.MaxObjectSize
	require.Error(t, bad.validate(), "ciphertext overhead counts against object cap")
	bad = *cfg
	bad.Remote = ":local:/tmp"
	require.Error(t, bad.validate())
	bad = *cfg
	bad.Version = 2
	require.Error(t, bad.validate())
	ctx, ci := fs.AddConfig(ctx)
	ci.Dump = fs.DumpBodies
	require.Error(t, safeLogging(ctx))

	lock, err := lockCache(cfg)
	require.NoError(t, err)
	_, err = lockCache(cfg)
	require.Error(t, err)
	require.NoError(t, lock.Close())
	lock, err = lockCache(cfg)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	base, _ := localStack(t, cfg)
	require.NoError(t, bindCache(cfg, base))
	require.NoError(t, bindCache(cfg, base))
	bad = *cfg
	bad.Password = "another password"
	require.Error(t, bindCache(&bad, base), "pending plaintext cannot be silently moved to a different vault")
	metadataDir := filepath.Join(cfg.CacheDir, "vfsMeta", cfg.cacheName())
	require.NoError(t, os.MkdirAll(metadataDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(metadataDir, "pending"), []byte(`{"Dirty":true,"Size":1}`), 0600))
	require.NoError(t, os.Remove(filepath.Join(cfg.CacheDir, cfg.cacheName()+".identity")))
	require.Error(t, bindCache(&bad, base), "missing identity must not allow pending plaintext to be rebound")
}

func TestMountSettingsValidationAndFlagPrecedence(t *testing.T) {
	writeSettings := func(t *testing.T, content string) string {
		t.Helper()
		filename := filepath.Join(t.TempDir(), "private", "mount.json")
		require.NoError(t, auth.WritePrivateFile(filename, []byte(content)))
		return filename
	}

	filename := writeSettings(t, `{"cache_max_size":"20G","cache_min_free_space":"20G","cache_max_age":"10m","cache_poll_interval":"5s","verify_upload":false}`)
	settings, err := readMountSettings(filename)
	require.NoError(t, err)
	options := vfscommon.Options{
		CacheMaxSize:      30 * fs.Gibi,
		CacheMinFreeSpace: fs.Gibi,
		CacheMaxAge:       fs.Duration(2 * time.Hour),
		CachePollInterval: fs.Duration(time.Minute),
	}
	flags := pflag.NewFlagSet("mount", pflag.ContinueOnError)
	flags.String("vfs-cache-max-size", "", "")
	flags.String("vfs-cache-min-free-space", "", "")
	flags.String("vfs-cache-max-age", "", "")
	flags.String("vfs-cache-poll-interval", "", "")
	flags.Bool("verify-upload", true, "")
	require.NoError(t, flags.Set("vfs-cache-max-size", "30G"))
	require.NoError(t, flags.Set("verify-upload", "true"))
	verifyUpload, err := settings.apply(&options, flags, true)
	require.NoError(t, err)
	require.True(t, verifyUpload, "explicit command-line value should override settings JSON")
	require.Equal(t, 30*fs.Gibi, options.CacheMaxSize, "explicit command-line value should override settings JSON")
	require.Equal(t, 20*fs.Gibi, options.CacheMinFreeSpace)
	require.Equal(t, fs.Duration(10*time.Minute), options.CacheMaxAge)
	require.Equal(t, fs.Duration(5*time.Second), options.CachePollInterval)

	empty, err := readMountSettings("")
	require.NoError(t, err)
	defaults := vfscommon.Options{CacheMaxAge: fs.Duration(time.Hour), CachePollInterval: fs.Duration(time.Minute)}
	verifyUpload, err = empty.apply(&defaults, nil, false)
	require.NoError(t, err)
	require.True(t, verifyUpload, "verification defaults on when settings and flags are absent")
	require.Equal(t, fs.Duration(time.Hour), defaults.CacheMaxAge)
	require.Equal(t, fs.Duration(time.Minute), defaults.CachePollInterval)
}

func TestMountSettingsDefaultsAndNewValues(t *testing.T) {
	apply := func(settings *mountSettings, flags *pflag.FlagSet) (vfscommon.Options, mountlib.Options, fs.ConfigInfo, error) {
		vfsOptions := vfscommon.Options{
			ChunkSize:      128 * fs.Mebi,
			ChunkSizeLimit: -1,
			DirCacheTime:   fs.Duration(5 * time.Minute),
		}
		mountOptions := mountlib.Options{AttrTimeout: fs.Duration(time.Second)}
		configOptions := fs.ConfigInfo{BufferSize: 16 * fs.Mebi, Transfers: 4, Timeout: fs.Duration(5 * time.Minute)}
		_, err := settings.applyWithConfig(&vfsOptions, &mountOptions, &configOptions, flags, true)
		return vfsOptions, mountOptions, configOptions, err
	}

	empty, err := readMountSettings("")
	require.NoError(t, err)
	vfsOptions, mountOptions, configOptions, err := apply(empty, nil)
	require.NoError(t, err)
	require.Equal(t, 16*fs.Mebi, configOptions.BufferSize)
	require.Equal(t, fs.Mebi, vfsOptions.ReadAhead)
	require.Equal(t, 128*fs.Mebi, vfsOptions.ChunkSize)
	require.Equal(t, fs.SizeSuffix(-1), vfsOptions.ChunkSizeLimit)
	require.Equal(t, 8, vfsOptions.ChunkStreams)
	require.Equal(t, fs.Duration(30*time.Minute), vfsOptions.DirCacheTime)
	require.Equal(t, fs.Duration(time.Second), mountOptions.AttrTimeout)
	require.Equal(t, 4, configOptions.Transfers)
	require.Empty(t, configOptions.BwLimit, "an empty bandwidth timetable means unlimited")
	require.Equal(t, fs.Duration(5*time.Minute), configOptions.Timeout)

	legacyFilename := filepath.Join(t.TempDir(), "private", "mount.json")
	require.NoError(t, auth.WritePrivateFile(legacyFilename, []byte(`{"cache_max_age":"10m","write_back":"17s","verify_upload":false}`)))
	legacySettings, err := readMountSettings(legacyFilename)
	require.NoError(t, err)
	legacyVFSOptions, legacyMountOptions, legacyConfigOptions, err := apply(legacySettings, nil)
	require.NoError(t, err)
	require.Equal(t, fs.Mebi, legacyVFSOptions.ReadAhead)
	require.Equal(t, 128*fs.Mebi, legacyVFSOptions.ChunkSize)
	require.Equal(t, fs.SizeSuffix(-1), legacyVFSOptions.ChunkSizeLimit)
	require.Equal(t, 8, legacyVFSOptions.ChunkStreams)
	require.Equal(t, fs.Duration(30*time.Minute), legacyVFSOptions.DirCacheTime)
	require.Equal(t, fs.Duration(time.Second), legacyMountOptions.AttrTimeout)
	require.Equal(t, fs.Duration(17*time.Second), legacyVFSOptions.WriteBack)
	require.Equal(t, fs.Duration(10*time.Minute), legacyVFSOptions.CacheMaxAge)
	require.Equal(t, 4, legacyConfigOptions.Transfers)
	require.Equal(t, fs.Duration(5*time.Minute), legacyConfigOptions.Timeout)

	filename := filepath.Join(t.TempDir(), "private", "mount.json")
	require.NoError(t, auth.WritePrivateFile(filename, []byte(`{"buffer_size":"32Mi","read_ahead":"2Mi","read_chunk_size":"64Mi","read_chunk_size_limit":"1Gi","read_chunk_streams":3,"dir_cache_time":"45m","attr_timeout":"250ms","transfers":6,"bwlimit":"off","timeout":"7m"}`)))
	settings, err := readMountSettings(filename)
	require.NoError(t, err)
	vfsOptions, mountOptions, configOptions, err = apply(settings, nil)
	require.NoError(t, err)
	require.Equal(t, 32*fs.Mebi, configOptions.BufferSize)
	require.Equal(t, 2*fs.Mebi, vfsOptions.ReadAhead)
	require.Equal(t, 64*fs.Mebi, vfsOptions.ChunkSize)
	require.Equal(t, fs.Gibi, vfsOptions.ChunkSizeLimit)
	require.Equal(t, 3, vfsOptions.ChunkStreams)
	require.Equal(t, fs.Duration(45*time.Minute), vfsOptions.DirCacheTime)
	require.Equal(t, fs.Duration(250*time.Millisecond), mountOptions.AttrTimeout)
	require.Equal(t, 6, configOptions.Transfers)
	require.Len(t, configOptions.BwLimit, 1)
	bandwidth := configOptions.BwLimit.LimitAt(time.Now()).Bandwidth
	require.False(t, bandwidth.IsSet(), `"off" is the canonical unlimited value`)
	require.Equal(t, fs.Duration(7*time.Minute), configOptions.Timeout)
}

func TestMountSettingsNewCLIFlagPrecedence(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "private", "mount.json")
	content := `{"buffer_size":"32Mi","read_ahead":"2Mi","read_chunk_size":"64Mi","read_chunk_size_limit":"1Gi","read_chunk_streams":3,"dir_cache_time":"45m","attr_timeout":"250ms","transfers":6,"bwlimit":"off","timeout":"7m"}`
	require.NoError(t, auth.WritePrivateFile(filename, []byte(content)))
	settings, err := readMountSettings(filename)
	require.NoError(t, err)

	flags := pflag.NewFlagSet("mount", pflag.ContinueOnError)
	for _, name := range []string{
		"buffer-size", "vfs-read-ahead", "vfs-read-chunk-size", "vfs-read-chunk-size-limit",
		"vfs-read-chunk-streams", "dir-cache-time", "attr-timeout", "transfers", "bwlimit", "timeout",
	} {
		flags.String(name, "", "")
		require.NoError(t, flags.Set(name, "cli-value"))
	}

	vfsOptions := vfscommon.Options{
		ReadAhead:      7 * fs.Mebi,
		ChunkSize:      96 * fs.Mebi,
		ChunkSizeLimit: 2 * fs.Gibi,
		ChunkStreams:   5,
		DirCacheTime:   fs.Duration(90 * time.Minute),
	}
	mountOptions := mountlib.Options{AttrTimeout: fs.Duration(3 * time.Second)}
	configOptions := fs.ConfigInfo{
		BufferSize: 64 * fs.Mebi,
		Transfers:  9,
		BwLimit:    fs.BwTimetable{},
		Timeout:    fs.Duration(11 * time.Minute),
	}
	_, err = settings.applyWithConfig(&vfsOptions, &mountOptions, &configOptions, flags, true)
	require.NoError(t, err)
	require.Equal(t, 7*fs.Mebi, vfsOptions.ReadAhead)
	require.Equal(t, 96*fs.Mebi, vfsOptions.ChunkSize)
	require.Equal(t, 2*fs.Gibi, vfsOptions.ChunkSizeLimit)
	require.Equal(t, 5, vfsOptions.ChunkStreams)
	require.Equal(t, fs.Duration(90*time.Minute), vfsOptions.DirCacheTime)
	require.Equal(t, fs.Duration(3*time.Second), mountOptions.AttrTimeout)
	require.Equal(t, 64*fs.Mebi, configOptions.BufferSize)
	require.Equal(t, 9, configOptions.Transfers)
	require.Empty(t, configOptions.BwLimit)
	require.Equal(t, fs.Duration(11*time.Minute), configOptions.Timeout)
}

func TestMountSettingsNewInvalidValuesAreRejectedWithoutGlobalMutation(t *testing.T) {
	for _, content := range []string{
		`{"buffer_size":"not-a-size"}`,
		`{"buffer_size":"off"}`,
		`{"read_ahead":"not-a-size"}`,
		`{"read_ahead":"off"}`,
		`{"read_chunk_size":"not-a-size"}`,
		`{"read_chunk_size_limit":"not-a-size"}`,
		`{"read_chunk_streams":-1}`,
		`{"dir_cache_time":"not-a-duration"}`,
		`{"attr_timeout":"not-a-duration"}`,
		`{"transfers":0}`,
		`{"bwlimit":"not-a-bandwidth"}`,
		`{"timeout":"not-a-duration"}`,
	} {
		filename := filepath.Join(t.TempDir(), "private", "mount.json")
		require.NoError(t, auth.WritePrivateFile(filename, []byte(content)))
		settings, err := readMountSettings(filename)
		require.NoError(t, err)
		_, globalOptions := fs.AddConfig(context.Background())
		before := *fs.GetConfig(context.Background())
		_, err = settings.applyWithConfig(&vfscommon.Options{}, &mountlib.Options{}, globalOptions, nil, true)
		require.Error(t, err, "invalid mount setting %s must be rejected", content)
		require.Equal(t, before, *fs.GetConfig(context.Background()), "failed validation must not mutate global config")
	}
}

func TestMountSettingsBandwidthChangesRuntimeLimiter(t *testing.T) {
	settings := &mountSettings{BwLimit: stringPointer("2Mi")}
	configOptions := &fs.ConfigInfo{}
	_, err := settings.applyWithConfig(&vfscommon.Options{}, &mountlib.Options{}, configOptions, nil, true)
	require.NoError(t, err)
	call := rc.Calls.Get("core/bwlimit")
	require.NotNil(t, call)
	before, err := call.Fn(context.Background(), rc.Params{})
	require.NoError(t, err)
	oldSchedule := append(fs.BwTimetable(nil), fs.GetConfig(context.Background()).BwLimit...)
	restore, err := applyMountBandwidth(settings, nil, configOptions)
	require.NoError(t, err)
	require.Equal(t, oldSchedule, fs.GetConfig(context.Background()).BwLimit, "the mount must not mutate global config")
	out, err := call.Fn(context.Background(), rc.Params{})
	require.NoError(t, err)
	require.Equal(t, "2Mi", out["rate"])
	restore()
	out, err = call.Fn(context.Background(), rc.Params{})
	require.NoError(t, err)
	require.Equal(t, before["rate"], out["rate"], "the previous runtime limit must be restored")
}

func TestMountBandwidthTickerUpdatesAndStops(t *testing.T) {
	var schedule fs.BwTimetable
	require.NoError(t, schedule.Set("Sun-00:00,1Mi Mon-00:00,2Mi"))
	sunday := time.Date(2025, time.January, 5, 0, 0, 0, 0, time.UTC)
	monday := sunday.Add(24 * time.Hour)
	initial := schedule.LimitAt(sunday).Bandwidth
	require.Equal(t, fs.BwPair{Tx: fs.Mebi, Rx: fs.Mebi}, initial)

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	updates := make(chan fs.BwPair, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runMountBandwidthTicker(ctx, schedule, initial, ticks, func(limit fs.BwPair) { updates <- limit })
	}()
	ticks <- monday
	select {
	case got := <-updates:
		require.Equal(t, fs.BwPair{Tx: 2 * fs.Mebi, Rx: 2 * fs.Mebi}, got)
	case <-time.After(time.Second):
		t.Fatal("bandwidth schedule did not update")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bandwidth schedule goroutine did not stop")
	}
}

func TestMountSettingsWriteBack(t *testing.T) {
	writeSettings := func(t *testing.T, content string) string {
		t.Helper()
		filename := filepath.Join(t.TempDir(), "private", "mount.json")
		require.NoError(t, auth.WritePrivateFile(filename, []byte(content)))
		return filename
	}

	t.Run("settings value is parsed", func(t *testing.T) {
		settings, err := readMountSettings(writeSettings(t, `{"write_back":"17s"}`))
		require.NoError(t, err)
		options := vfscommon.Options{WriteBack: fs.Duration(5 * time.Second)}
		verifyUpload, err := settings.apply(&options, nil, true)
		require.NoError(t, err)
		require.True(t, verifyUpload)
		require.Equal(t, fs.Duration(17*time.Second), options.WriteBack)
	})

	t.Run("explicit VFS flag takes precedence", func(t *testing.T) {
		settings, err := readMountSettings(writeSettings(t, `{"write_back":"17s"}`))
		require.NoError(t, err)
		options := vfscommon.Options{WriteBack: fs.Duration(9 * time.Second)}
		flags := pflag.NewFlagSet("mount", pflag.ContinueOnError)
		flags.String("vfs-write-back", "", "")
		require.NoError(t, flags.Set("vfs-write-back", "9s"))
		_, err = settings.apply(&options, flags, true)
		require.NoError(t, err)
		require.Equal(t, fs.Duration(9*time.Second), options.WriteBack)
	})

	t.Run("positive VFS flag overrides invalid saved value", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			content string
		}{
			{name: "zero", content: `{"write_back":"0s"}`},
			{name: "off", content: `{"write_back":"off"}`},
			{name: "malformed", content: `{"write_back":"not-a-duration"}`},
		} {
			t.Run(test.name, func(t *testing.T) {
				settings, err := readMountSettings(writeSettings(t, test.content))
				require.NoError(t, err)
				options := vfscommon.Options{WriteBack: fs.Duration(9 * time.Second)}
				flags := pflag.NewFlagSet("mount", pflag.ContinueOnError)
				flags.String("vfs-write-back", "", "")
				require.NoError(t, flags.Set("vfs-write-back", "9s"))
				_, err = settings.apply(&options, flags, true)
				require.NoError(t, err)
				require.Equal(t, fs.Duration(9*time.Second), options.WriteBack)
				require.NoError(t, validateWriteBack(options.WriteBack))
			})
		}
	})

	t.Run("missing setting preserves defaults", func(t *testing.T) {
		settings, err := readMountSettings(writeSettings(t, `{}`))
		require.NoError(t, err)
		options := vfscommon.Options{
			WriteBack:         fs.Duration(5 * time.Second),
			CacheMaxAge:       fs.Duration(time.Hour),
			CachePollInterval: fs.Duration(time.Minute),
		}
		_, err = settings.apply(&options, nil, true)
		require.NoError(t, err)
		require.Equal(t, fs.Duration(5*time.Second), options.WriteBack)
		require.Equal(t, fs.Duration(time.Hour), options.CacheMaxAge)
		require.Equal(t, fs.Duration(time.Minute), options.CachePollInterval)
	})
}

func TestMountWriteBackMustBePositiveAndFinite(t *testing.T) {
	for _, value := range []fs.Duration{
		fs.Duration(0),
		fs.Duration(-time.Second),
		fs.DurationOff,
	} {
		require.Error(t, validateWriteBack(value))
	}
	require.NoError(t, validateWriteBack(fs.Duration(5*time.Second)))

	options := vfscommon.Options{WriteBack: fs.Duration(0)}
	flags := pflag.NewFlagSet("mount", pflag.ContinueOnError)
	flags.String("vfs-write-back", "", "")
	require.NoError(t, flags.Set("vfs-write-back", "0s"))
	settings := &mountSettings{WriteBack: stringPointer("17s")}
	_, err := settings.apply(&options, flags, true)
	require.NoError(t, err, "an explicit flag should still take precedence over settings")
	require.Error(t, validateWriteBack(options.WriteBack), "the effective VFS value must remain positive")
}

func stringPointer(value string) *string { return &value }

func TestMountSettingsRejectUnknownAndInvalidValues(t *testing.T) {
	writeSettings := func(content string) string {
		filename := filepath.Join(t.TempDir(), "private", "mount.json")
		require.NoError(t, auth.WritePrivateFile(filename, []byte(content)))
		return filename
	}

	_, err := readMountSettings(writeSettings(`{"unknown":true}`))
	require.Error(t, err, "unknown fields are rejected")
	_, err = readMountSettings(writeSettings(`{} {}`))
	require.Error(t, err, "trailing JSON is rejected")

	flags := pflag.NewFlagSet("mount", pflag.ContinueOnError)
	flags.String("vfs-cache-max-size", "", "")
	require.NoError(t, flags.Set("vfs-cache-max-size", "30G"))
	for _, content := range []string{
		`{"cache_max_size":"not-a-size"}`,
		`{"cache_max_age":"not-a-duration"}`,
		`{"cache_poll_interval":"not-a-duration"}`,
		`{"write_back":"not-a-duration"}`,
		`{"write_back":"0s"}`,
		`{"write_back":"-1s"}`,
		`{"write_back":"off"}`,
	} {
		settings, readErr := readMountSettings(writeSettings(content))
		require.NoError(t, readErr)
		_, err = settings.apply(&vfscommon.Options{}, flags, true)
		require.Error(t, err, "invalid settings are rejected even when a CLI flag overrides the field")
	}
}

func TestMountRawConfigScopesUploadVerification(t *testing.T) {
	remote := ":degoo,auth_file=/tmp/degoo-auth.json,root_folder_id=vault,skip_upload_verification=true:dir"
	for _, test := range []struct {
		verify bool
		want   string
	}{{verify: true, want: "false"}, {verify: false, want: "true"}} {
		fsInfo, configName, fsPath, config, err := mountRawConfig(remote, test.verify)
		require.NoError(t, err)
		require.Equal(t, "degoo", fsInfo.Name)
		require.Equal(t, ":degoo", configName)
		require.Equal(t, "dir", fsPath)
		got, ok := config.Get("skip_upload_verification")
		require.True(t, ok)
		require.Equal(t, test.want, got)
		authFile, ok := config.Get("auth_file")
		require.True(t, ok)
		require.Equal(t, "/tmp/degoo-auth.json", authFile)
	}
}

func TestLoginPasswordPromptShowsCachedEmailRecovery(t *testing.T) {
	require.Equal(t, "Degoo account password for user@example.com: ", loginPasswordPrompt("user@example.com", false))
	require.Equal(t, "Degoo account password for user@example.com (cached email; clear with ~/.local/share/rclone-degoo/login.sh --clear-email): ", loginPasswordPrompt("user@example.com", true))
}

func TestPromptPasswordUsesPromptOutputWhenStderrRedirected(t *testing.T) {
	master, slave := openPTY(t)
	logFile, err := os.CreateTemp(t.TempDir(), "degoo-login.log")
	require.NoError(t, err)

	stdin, stdout, stderr, promptOutput := os.Stdin, os.Stdout, os.Stderr, config.PasswordPromptOutput
	os.Stdin, os.Stdout, os.Stderr = slave, logFile, logFile
	config.PasswordPromptOutput = slave
	t.Cleanup(func() {
		os.Stdin, os.Stdout, os.Stderr = stdin, stdout, stderr
		config.PasswordPromptOutput = promptOutput
		_ = logFile.Close()
	})

	const password = "generated test password"
	type result struct {
		password string
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		password, err := promptPassword("Password: ")
		resultCh <- result{password: password, err: err}
	}()

	const prompt = "Password: "
	shownPrompt := make([]byte, len(prompt))
	promptRead := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(master, shownPrompt)
		promptRead <- err
	}()
	select {
	case err := <-promptRead:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("password prompt was not written to the terminal")
	}
	require.Equal(t, prompt, string(shownPrompt))

	deadline := time.Now().Add(2 * time.Second)
	for {
		state, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
		require.NoError(t, err)
		if state.Lflag&unix.ECHO == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("password input was not hidden")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err = master.Write([]byte(password + "\n"))
	require.NoError(t, err)

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("password input did not finish")
	}
	require.NoError(t, got.err)
	require.True(t, got.password == password)

	pending, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCINQ)
	require.NoError(t, err)
	terminalOutput := append([]byte(nil), shownPrompt...)
	if pending > 0 {
		trailing := make([]byte, pending)
		_, err := io.ReadFull(master, trailing)
		require.NoError(t, err)
		terminalOutput = append(terminalOutput, trailing...)
	}
	require.NotContains(t, string(terminalOutput), password)

	_, err = logFile.Seek(0, io.SeekStart)
	require.NoError(t, err)
	logOutput, err := io.ReadAll(logFile)
	require.NoError(t, err)
	require.NotContains(t, string(logOutput), prompt)
	require.NotContains(t, string(logOutput), password)
}

func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close() })
	require.NoError(t, unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0))
	ptyNumber, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	require.NoError(t, err)
	slave, err = os.OpenFile(filepath.Join("/dev/pts", strconv.Itoa(ptyNumber)), os.O_RDWR|unix.O_NOCTTY, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

func TestVaultEncryptsChunksNamesAndManifests(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	base, stack := localStack(t, cfg)
	data := bytes.Repeat([]byte("generated plaintext contents "), 200)
	obj := put(t, stack, "private-dir/private-file.txt", data)
	require.Equal(t, data, read(t, obj))
	require.Equal(t, data[950:2150], read(t, obj, &fs.RangeOption{Start: 950, End: 2149}))
	root, err := base.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, root, 1)
	require.NotContains(t, root[0].Remote(), "private")
	entries, err := base.List(ctx, root[0].Remote())
	require.NoError(t, err)
	require.Len(t, entries, (len(data)+1023)/1024+1)
	for _, entry := range entries {
		require.NotContains(t, entry.Remote(), "private")
		require.NotContains(t, entry.Remote(), "rclone_chunk")
		encoded := read(t, entry.(fs.Object))
		require.True(t, bytes.HasPrefix(encoded, []byte("RCLONE")), "data and manifest both use the crypt format")
		require.NotContains(t, string(encoded), "generated plaintext contents")
		require.NotContains(t, string(encoded), "nchunks")
		require.LessOrEqual(t, int64(len(encoded)), int64(cfg.MaxObjectSize))
	}

	// A fresh overlay must recover the mapping entirely from encrypted metadata.
	remounted, err := newStack(ctx, base, cfg)
	require.NoError(t, err)
	obj, err = remounted.NewObject(ctx, "private-dir/private-file.txt")
	require.NoError(t, err)
	require.Equal(t, data, read(t, obj))
	moved, err := remounted.Features().Move(ctx, obj, "private-dir/renamed.txt")
	require.NoError(t, err)
	require.Equal(t, data, read(t, moved))
	require.NoError(t, moved.Remove(ctx))
	require.NoError(t, remounted.Rmdir(ctx, "private-dir"))
}

func TestVaultRejectsWrongPasswordAndDamagedChunks(t *testing.T) {
	for _, damage := range []string{"corrupt", "missing", "wrong-password"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			cfg := testConfig(t)
			base, stack := localStack(t, cfg)
			data := bytes.Repeat([]byte("generated data "), 500)
			put(t, stack, "test", data)
			if damage == "wrong-password" {
				cfg.Password = "wrong vault secret"
				wrong, err := newStack(ctx, base, cfg)
				require.NoError(t, err)
				_, err = wrong.NewObject(ctx, "test")
				require.Error(t, err)
				return
			}
			entries, err := base.List(ctx, "")
			require.NoError(t, err)
			var chunk fs.Object
			for _, entry := range entries {
				candidate := entry.(fs.Object)
				if candidate.Size() > 1000 {
					chunk = candidate
					break
				}
			}
			require.NotNil(t, chunk)
			if damage == "missing" {
				require.NoError(t, chunk.Remove(ctx))
			} else {
				ciphertext := read(t, chunk)
				ciphertext[len(ciphertext)-1] ^= 1
				put(t, base, chunk.Remote(), ciphertext)
			}
			fresh, err := newStack(ctx, base, cfg)
			require.NoError(t, err)
			obj, err := fresh.NewObject(ctx, "test")
			if err == nil {
				r, openErr := obj.Open(ctx)
				if openErr == nil {
					_, err = io.ReadAll(r)
					require.NoError(t, r.Close())
				} else {
					err = openErr
				}
			}
			require.Error(t, err, "damage must fail the whole read")
		})
	}
}

func TestVaultPendingWriteSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	base, stack := localStack(t, cfg)
	oldCache := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cfg.CacheDir))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCache)) })
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.WriteBack = fs.Duration(time.Hour)
	options.PollInterval = 0
	v := vfs.New(ctx, stack, &options)
	fd, err := v.OpenFile("pending-file", os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	data := []byte("generated locally saved write")
	_, err = fd.Write(data)
	require.NoError(t, err)
	require.NoError(t, fd.Sync())
	require.NoError(t, fd.Close())
	files, size, err := pending(cfg)
	require.NoError(t, err)
	require.Equal(t, int64(1), files)
	require.Equal(t, int64(len(data)), size)
	_, err = base.NewObject(ctx, "pending-file")
	require.ErrorIs(t, err, fs.ErrorObjectNotFound)
	v.Shutdown()

	stack, err = newStack(ctx, base, cfg)
	require.NoError(t, err)
	options.WriteBack = fs.Duration(20 * time.Millisecond)
	v = vfs.New(ctx, stack, &options)
	defer v.Shutdown()
	require.Eventually(t, func() bool {
		obj, err := stack.NewObject(ctx, "pending-file")
		if err != nil {
			return false
		}
		r, err := obj.Open(ctx)
		if err != nil {
			return false
		}
		got, readErr := io.ReadAll(r)
		_ = r.Close()
		return readErr == nil && bytes.Equal(got, data)
	}, 10*time.Second, 20*time.Millisecond)
	// Remote publication precedes the asynchronous clean-cache metadata update.
	require.Eventually(t, func() bool {
		files, _, err := pending(cfg)
		return err == nil && files == 0
	}, 10*time.Second, 20*time.Millisecond)
}
