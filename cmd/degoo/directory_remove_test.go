//go:build linux

package degoo

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/chunker"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

func TestVaultRemoveAllNestedDirectory(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	base, stack := localStack(t, cfg)
	require.NoError(t, stack.Mkdir(ctx, "remove-parent"))
	require.NoError(t, stack.Mkdir(ctx, "remove-parent/nested"))
	put(t, stack, "remove-parent/nested/clean-file", []byte("generated nested encrypted file"))

	oldCache := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cfg.CacheDir))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCache)) })
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.PollInterval = 0
	mounted := vfs.New(ctx, stack, &options)
	t.Cleanup(mounted.Shutdown)

	node, err := mounted.Stat("remove-parent")
	require.NoError(t, err)
	require.IsType(t, (*vfs.Dir)(nil), node)
	require.NoError(t, node.RemoveAll())

	_, err = stack.NewObject(ctx, "remove-parent/nested/clean-file")
	require.ErrorIs(t, err, fs.ErrorObjectNotFound)
	entries, err := stack.List(ctx, "")
	require.NoError(t, err)
	require.Empty(t, entries, "recursive VFS removal must clear the encrypted directory tree")

	remounted, err := newStack(ctx, base, cfg)
	require.NoError(t, err)
	entries, err = remounted.List(ctx, "")
	require.NoError(t, err)
	require.Empty(t, entries, "removed directories must stay absent after reopening the encrypted stack")
}

func TestVaultNativeDirectoryRemoval(t *testing.T) {
	if !*degooFUSETest {
		t.Skip("pass -degoo-fuse-test to run the native Linux FUSE smoke test")
	}
	ctx := context.Background()
	cfg := testConfig(t)
	_, stack := localStack(t, cfg)
	oldCache := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cfg.CacheDir))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCache)) })
	mountDir := filepath.Join(t.TempDir(), "mount")
	require.NoError(t, os.Mkdir(mountDir, 0700))
	mountOptions := mountlib.Opt
	mountOptions.Daemon = false
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.WriteBack = fs.Duration(time.Hour)
	options.PollInterval = 0
	mount := startVaultMount(ctx, t, mountDir, stack, &mountOptions, &options)
	t.Cleanup(func() { stopVaultMount(t, mount) })

	dirtyDir := filepath.Join(mountDir, "dirty-dir")
	require.NoError(t, os.Mkdir(dirtyDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dirtyDir, "pending"), []byte("generated pending file"), 0600))
	files, _, err := pending(cfg)
	require.NoError(t, err)
	require.Equal(t, int64(1), files)
	require.ErrorIs(t, os.Remove(dirtyDir), syscall.ENOTEMPTY)
	require.NoError(t, os.RemoveAll(dirtyDir))
	files, _, err = pending(cfg)
	require.NoError(t, err)
	require.Zero(t, files)

	orphanDir := filepath.Join(mountDir, "orphan-dir")
	require.NoError(t, os.Mkdir(orphanDir, 0700))
	cipher := stack.(*chunker.Fs).UnWrap()
	name := "orphan-dir/interrupted.rclone_chunk.001_abcd"
	_, err = cipher.Put(ctx, bytes.NewReader(nil), object.NewStaticObjectInfo(name, time.Now(), 0, true, nil, cipher))
	require.NoError(t, err)
	entries, err := os.ReadDir(orphanDir)
	require.NoError(t, err)
	require.Empty(t, entries)
	require.NoError(t, os.Remove(orphanDir), "rmdir must clean an orphan hidden by the encrypted chunker")
}

func TestVaultRemoveAllCancelsDirtyNestedWriteAcrossRemount(t *testing.T) {
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
	mounted := vfs.New(ctx, stack, &options)
	mountedActive := true
	t.Cleanup(func() {
		if mountedActive {
			mounted.Shutdown()
		}
	})

	require.NoError(t, mounted.MkdirAll("dirty-parent/nested", 0700))
	fd, err := mounted.OpenFile("dirty-parent/nested/pending-file", os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	data := []byte("generated pending nested write")
	_, err = fd.Write(data)
	require.NoError(t, err)
	require.NoError(t, fd.Sync())
	require.NoError(t, fd.Close())
	files, size, err := pending(cfg)
	require.NoError(t, err)
	require.Equal(t, int64(1), files)
	require.Equal(t, int64(len(data)), size)
	_, err = stack.NewObject(ctx, "dirty-parent/nested/pending-file")
	require.ErrorIs(t, err, fs.ErrorObjectNotFound)

	node, err := mounted.Stat("dirty-parent")
	require.NoError(t, err)
	require.IsType(t, (*vfs.Dir)(nil), node)
	require.NoError(t, node.RemoveAll())
	files, size, err = pending(cfg)
	require.NoError(t, err)
	require.Zero(t, files, "removing the directory must cancel its queued upload")
	require.Zero(t, size)
	_, err = stack.NewObject(ctx, "dirty-parent/nested/pending-file")
	require.ErrorIs(t, err, fs.ErrorObjectNotFound)

	mounted.Shutdown()
	mountedActive = false
	stack, err = newStack(ctx, base, cfg)
	require.NoError(t, err)
	options.WriteBack = fs.Duration(20 * time.Millisecond)
	remounted := vfs.New(ctx, stack, &options)
	remountedActive := true
	t.Cleanup(func() {
		if remountedActive {
			remounted.Shutdown()
		}
	})
	_, err = remounted.Stat("dirty-parent")
	require.ErrorIs(t, err, vfs.ENOENT)

	require.Never(t, func() bool {
		_, err := stack.NewObject(ctx, "dirty-parent/nested/pending-file")
		return err == nil
	}, 500*time.Millisecond, 20*time.Millisecond, "a removed dirty file must not be uploaded after remount")
	files, size, err = pending(cfg)
	require.NoError(t, err)
	require.Zero(t, files)
	require.Zero(t, size)
}

func TestVaultRmdirRejectsVisibleCleanAndDirtyFiles(t *testing.T) {
	for _, state := range []string{"clean", "dirty"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			cfg := testConfig(t)
			_, stack := localStack(t, cfg)
			oldCache := config.GetCacheDir()
			require.NoError(t, config.SetCacheDir(cfg.CacheDir))
			t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCache)) })

			options := vfscommon.Opt
			options.CacheMode = vfscommon.CacheModeFull
			options.WriteBack = fs.Duration(time.Hour)
			options.PollInterval = 0
			mounted := vfs.New(ctx, stack, &options)
			t.Cleanup(mounted.Shutdown)

			const dir = "visible-dir"
			const file = dir + "/visible-file"
			if state == "clean" {
				require.NoError(t, stack.Mkdir(ctx, dir))
				put(t, stack, file, []byte("generated uploaded file"))
			} else {
				require.NoError(t, mounted.MkdirAll(dir, 0700))
				fd, err := mounted.OpenFile(file, os.O_CREATE|os.O_RDWR, 0600)
				require.NoError(t, err)
				_, err = fd.Write([]byte("generated pending file"))
				require.NoError(t, err)
				require.NoError(t, fd.Sync())
				require.NoError(t, fd.Close())
			}

			err := mounted.Remove(dir)
			require.ErrorIs(t, err, vfs.ENOTEMPTY, "rmdir must reject a directory with a visible file")
			_, err = mounted.Stat(file)
			require.NoError(t, err, "failed rmdir must leave the visible file in place")
		})
	}
}
