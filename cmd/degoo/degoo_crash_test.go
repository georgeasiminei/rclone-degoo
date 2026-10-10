//go:build linux

package degoo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

const (
	crashHelperEnv = "RCLONE_DEGOO_CRASH_HELPER"
	crashConfigEnv = "RCLONE_DEGOO_CRASH_CONFIG"
	crashBaseEnv   = "RCLONE_DEGOO_CRASH_BASE"
)

func TestVaultPendingWriteSurvivesProcessCrash(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "base")
	cacheDir := filepath.Join(root, "cache")
	require.NoError(t, os.Mkdir(baseDir, 0o700))
	require.NoError(t, os.Mkdir(cacheDir, 0o700))
	cfg := testConfig(t)
	cfg.CacheDir = cacheDir
	configData, err := json.Marshal(cfg)
	require.NoError(t, err)

	child := exec.Command(os.Args[0], "-test.run=^TestVaultCrashHelper$")
	child.Env = append(os.Environ(),
		crashHelperEnv+"=1",
		crashConfigEnv+"="+string(configData),
		crashBaseEnv+"="+baseDir,
	)
	output, err := child.CombinedOutput()
	require.NoError(t, err, "crash helper failed: %s", output)

	files, size, err := pending(cfg)
	require.NoError(t, err)
	require.Equal(t, int64(1), files)
	const data = "generated write recovered after abrupt process exit"
	require.Equal(t, int64(len(data)), size)

	ctx := context.Background()
	base, err := fs.NewFs(ctx, baseDir)
	require.NoError(t, err)
	stack, err := newStack(ctx, base, cfg)
	require.NoError(t, err)
	_, err = stack.NewObject(ctx, "crash-pending-file")
	require.ErrorIs(t, err, fs.ErrorObjectNotFound)
	oldCache := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cacheDir))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCache)) })
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.WriteBack = fs.Duration(20 * time.Millisecond)
	options.PollInterval = 0
	v := vfs.New(ctx, stack, &options)
	defer v.Shutdown()

	require.Eventually(t, func() bool {
		obj, err := stack.NewObject(ctx, "crash-pending-file")
		if err != nil {
			return false
		}
		r, err := obj.Open(ctx)
		if err != nil {
			return false
		}
		got, readErr := io.ReadAll(r)
		_ = r.Close()
		return readErr == nil && bytes.Equal(got, []byte(data))
	}, 10*time.Second, 20*time.Millisecond)

	// Remote publication precedes the asynchronous clean-cache metadata update.
	require.Eventually(t, func() bool {
		files, _, err := pending(cfg)
		return err == nil && files == 0
	}, 10*time.Second, 20*time.Millisecond)
}

func TestVaultCrashHelper(t *testing.T) {
	if os.Getenv(crashHelperEnv) != "1" {
		return
	}
	var cfg VaultConfig
	require.NoError(t, json.Unmarshal([]byte(os.Getenv(crashConfigEnv)), &cfg))
	baseDir := os.Getenv(crashBaseEnv)
	require.NotEmpty(t, baseDir)

	ctx := context.Background()
	base, err := fs.NewFs(ctx, baseDir)
	require.NoError(t, err)
	stack, err := newStack(ctx, base, &cfg)
	require.NoError(t, err)
	require.NoError(t, config.SetCacheDir(cfg.CacheDir))
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.WriteBack = fs.Duration(time.Hour)
	options.PollInterval = 0
	v := vfs.New(ctx, stack, &options)
	fd, err := v.OpenFile("crash-pending-file", os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	const data = "generated write recovered after abrupt process exit"
	_, err = fd.Write([]byte(data))
	require.NoError(t, err)
	require.NoError(t, fd.Sync())
	require.NoError(t, fd.Close())
	files, size, err := pending(&cfg)
	require.NoError(t, err)
	require.Equal(t, int64(1), files)
	require.Equal(t, int64(len(data)), size)
	_, err = stack.NewObject(ctx, "crash-pending-file")
	require.ErrorIs(t, err, fs.ErrorObjectNotFound)

	// Bypass Shutdown and deferred cleanup to model abrupt process termination.
	os.Exit(0)
}
