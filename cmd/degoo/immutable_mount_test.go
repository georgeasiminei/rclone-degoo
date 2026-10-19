//go:build linux

package degoo

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

var degooImmutableFUSETest = flag.Bool("degoo-immutable-fuse-test", false, "test the privileged helper on an isolated, generated immutable /mnt/degoo fixture")

func TestVaultNativeImmutableMount(t *testing.T) {
	if !*degooImmutableFUSETest {
		t.Skip("pass -degoo-immutable-fuse-test inside a disposable mount namespace with the helper installed")
	}
	require.NotZero(t, os.Getuid(), "run the client as an ordinary user")
	ctx := context.Background()
	cfg := testConfig(t)
	base, stack := localStack(t, cfg)
	oldCache := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cfg.CacheDir))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCache)) })
	const mountDir = "/mnt/degoo"
	filename := filepath.Join(mountDir, "generated-immutable-test.txt")
	data := []byte("generated encrypted file with an immutable underlying mountpoint")
	for pass := 0; pass < 2; pass++ {
		t.Logf("immutable mount pass %d", pass+1)
		guard, err := protectMountpoint(mountDir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = guard.file.Close() })
		require.True(t, guard.immutable)
		require.ErrorIs(t, os.WriteFile(filename, data, 0600), unix.EPERM)
		require.NoError(t, guard.prepare())
		_, mountFn := mountlib.ResolveMountMethod("mount")
		require.NotNil(t, mountFn)
		guardedMount := func(v *vfs.VFS, path string, opt *mountlib.Options) (<-chan error, func() error, string, error) {
			restore, err := guard.useImmutableMountHelper()
			if err != nil {
				return nil, nil, "", err
			}
			defer restore()
			return mountFn(v, path, opt)
		}
		mountOpt := mountlib.Opt
		mountOpt.Daemon = false
		vfsOpt := vfscommon.Opt
		vfsOpt.CacheMode = vfscommon.CacheModeFull
		vfsOpt.PollInterval = 0
		vfsOpt.WriteBack = fs.Duration(100 * time.Millisecond)
		mnt := mountlib.NewMountPoint(guardedMount, mountDir, stack, &mountOpt, &vfsOpt)
		mnt.Ctx = ctx
		_, err = mnt.Mount()
		require.NoError(t, err)
		active := true
		t.Cleanup(func() {
			if active {
				_ = mnt.Unmount()
			}
		})
		require.NoError(t, guard.protect())
		flags, err := unix.IoctlGetInt(int(guard.file.Fd()), unix.FS_IOC_GETFLAGS)
		require.NoError(t, err)
		require.NotZero(t, flags&fsImmutableFlag, "underlying directory stays immutable while mounted")
		if pass == 0 {
			require.NoError(t, os.WriteFile(filename, data, 0600))
			require.Eventually(t, func() bool {
				_, err := stack.NewObject(ctx, filepath.Base(filename))
				return err == nil
			}, 10*time.Second, 20*time.Millisecond)
		}
		got, err := os.ReadFile(filename)
		require.NoError(t, err)
		require.Equal(t, data, got)
		stopVaultMount(t, mnt)
		active = false
		require.NoError(t, guard.Close())
		require.ErrorIs(t, os.WriteFile(filename, data, 0600), unix.EPERM)
		stack, err = newStack(ctx, base, cfg)
		require.NoError(t, err)
	}
}
