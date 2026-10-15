//go:build linux

package degoo

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/rclone/rclone/cmd/mount"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

var degooFUSETest = flag.Bool("degoo-fuse-test", false, "run the opt-in native Degoo vault mount test")

func TestVaultNativeMount(t *testing.T) {
	if !*degooFUSETest {
		t.Skip("pass -degoo-fuse-test to run the native Linux FUSE smoke test")
	}

	ctx := context.Background()
	cfg := testConfig(t)
	base, stack := localStack(t, cfg)
	mountDir := filepath.Join(t.TempDir(), "mount")
	require.NoError(t, os.Mkdir(mountDir, 0o700))
	oldCache := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cfg.CacheDir))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCache)) })

	mountOpt := mountlib.Opt
	mountOpt.Daemon = false
	vfsOpt := vfscommon.Opt
	vfsOpt.CacheMode = vfscommon.CacheModeFull
	vfsOpt.WriteBack = 0
	vfsOpt.PollInterval = 0

	firstMount := startVaultMount(ctx, t, mountDir, stack, &mountOpt, &vfsOpt)
	firstActive := true
	t.Cleanup(func() {
		if firstActive {
			stopVaultMount(t, firstMount)
		}
	})

	dir := filepath.Join(mountDir, "native-ops")
	require.NoError(t, os.Mkdir(dir, 0o700))
	file := filepath.Join(dir, "operations.txt")
	initial := []byte("prefix-seek-target-suffix")
	require.NoError(t, os.WriteFile(file, initial, 0o600))
	got, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, initial, got)

	f, err := os.Open(file)
	require.NoError(t, err)
	_, err = f.Seek(int64(len("prefix-")), io.SeekStart)
	require.NoError(t, err)
	seeked := make([]byte, len("seek-target"))
	_, err = io.ReadFull(f, seeked)
	require.NoError(t, err)
	require.Equal(t, "seek-target", string(seeked))
	require.NoError(t, f.Close())

	truncated := []byte("prefix-OVERWRITE")
	writeFile, err := os.OpenFile(file, os.O_RDWR, 0)
	require.NoError(t, err)
	n, err := writeFile.WriteAt([]byte("OVERWRITE"), int64(len("prefix-")))
	require.NoError(t, err)
	require.Equal(t, len("OVERWRITE"), n)
	require.NoError(t, writeFile.Close())
	require.NoError(t, os.Truncate(file, int64(len(truncated))))
	got, err = os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, truncated, got)

	renamed := filepath.Join(dir, "renamed.txt")
	require.NoError(t, os.Rename(file, renamed))
	_, err = os.Stat(file)
	require.ErrorIs(t, err, os.ErrNotExist)
	got, err = os.ReadFile(renamed)
	require.NoError(t, err)
	require.Equal(t, truncated, got)
	require.NoError(t, os.Remove(renamed))
	_, err = os.Stat(renamed)
	require.ErrorIs(t, err, os.ErrNotExist)

	persisted := filepath.Join(mountDir, "persisted-after-remount.txt")
	persistedData := []byte("generated encrypted vault data survives unmount")
	require.NoError(t, os.WriteFile(persisted, persistedData, 0o600))
	require.Eventually(t, func() bool {
		obj, err := stack.NewObject(ctx, "persisted-after-remount.txt")
		if err != nil {
			return false
		}
		r, err := obj.Open(ctx)
		if err != nil {
			return false
		}
		got, readErr := io.ReadAll(r)
		closeErr := r.Close()
		return readErr == nil && closeErr == nil && string(got) == string(persistedData)
	}, 10*time.Second, 20*time.Millisecond, "pending generated file must reach local remote before unmount")
	stopVaultMount(t, firstMount)
	firstActive = false

	// Recreate the encrypted stack to model remounting after a backend restart.
	stack, err = newStack(ctx, base, cfg)
	require.NoError(t, err)
	secondMount := startVaultMount(ctx, t, mountDir, stack, &mountOpt, &vfsOpt)
	secondActive := true
	t.Cleanup(func() {
		if secondActive {
			stopVaultMount(t, secondMount)
		}
	})
	got, err = os.ReadFile(persisted)
	require.NoError(t, err)
	require.Equal(t, persistedData, got)
	stopVaultMount(t, secondMount)
	secondActive = false
}

func TestVaultUploadStatusNativeMount(t *testing.T) {
	if !*degooFUSETest {
		t.Skip("pass -degoo-fuse-test to run the native Linux FUSE smoke test")
	}
	ctx := context.Background()
	cfg := testConfig(t)
	_, stack := localStack(t, cfg)
	mountDir := filepath.Join(t.TempDir(), "mount")
	require.NoError(t, os.Mkdir(mountDir, 0700))
	oldCache := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cfg.CacheDir))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCache)) })
	guard, err := protectMountpoint(mountDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, guard.Close())
		require.NoError(t, os.Chmod(mountDir, 0700))
	})
	require.NoError(t, guard.prepare())
	mountOpt := mountlib.Opt
	vfsOpt := vfscommon.Opt
	vfsOpt.CacheMode = vfscommon.CacheModeFull
	vfsOpt.WriteBack = fs.Duration(500 * time.Millisecond)
	vfsOpt.PollInterval = 0
	vfsOpt.UploadStatus = true
	mnt := startVaultMount(ctx, t, mountDir, stack, &mountOpt, &vfsOpt)
	active := true
	t.Cleanup(func() {
		if active {
			stopVaultMount(t, mnt)
		}
	})
	require.NoError(t, guard.protect())
	filename := filepath.Join(mountDir, "status.txt")
	f, err := os.Create(filename)
	require.NoError(t, err)
	_, err = f.WriteString("generated upload state data")
	require.NoError(t, err)
	readStatus := func() string {
		value := make([]byte, 32)
		n, err := unix.Getxattr(filename, vfs.UploadStatusXattr, value)
		require.NoError(t, err)
		return string(value[:n])
	}
	require.Equal(t, "pending", readStatus())
	require.ErrorIs(t, unix.Setxattr(filename, vfs.UploadStatusXattr, []byte("uploaded"), 0), unix.EPERM)
	require.NoError(t, f.Close())
	require.Eventually(t, func() bool { return readStatus() == "uploaded" }, 10*time.Second, 20*time.Millisecond)
	stopVaultMount(t, mnt)
	active = false
	info, err := os.Stat(mountDir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0500), info.Mode().Perm())
	err = os.WriteFile(filepath.Join(mountDir, "unmounted.txt"), []byte("must fail"), 0600)
	require.ErrorIs(t, err, os.ErrPermission)
}

func startVaultMount(ctx context.Context, t *testing.T, mountDir string, remote fs.Fs, mountOpt *mountlib.Options, vfsOpt *vfscommon.Options) *mountlib.MountPoint {
	t.Helper()
	_, mountFn := mountlib.ResolveMountMethod("mount")
	require.NotNil(t, mountFn, "native Linux FUSE mount method should be registered")
	mountPoint := mountlib.NewMountPoint(mountFn, mountDir, remote, mountOpt, vfsOpt)
	mountPoint.Ctx = ctx
	_, err := mountPoint.Mount()
	require.NoError(t, err, "native FUSE mount failed; check /dev/fuse and fusermount access")
	return mountPoint
}

func stopVaultMount(t *testing.T, mountPoint *mountlib.MountPoint) {
	t.Helper()
	unmountErr := mountPoint.Unmount()
	var serverErr error
	select {
	case serverErr = <-mountPoint.ErrChan:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "timed out waiting for native FUSE server to stop")
	}
	require.NoError(t, unmountErr)
	require.NoError(t, serverErr)
}
