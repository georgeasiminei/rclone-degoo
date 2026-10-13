//go:build linux

package degoo

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/rclone/rclone/cmd/mount2"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

var degooUpdateFUSETest = flag.Bool("degoo-update-fuse-test", false, "test update preparation and pending-write recovery on a generated encrypted local FUSE vault")

func TestVaultNativeUpdatePreparation(t *testing.T) {
	if !*degooUpdateFUSETest {
		t.Skip("pass -degoo-update-fuse-test with FUSE available")
	}
	for _, method := range []string{"mount", "mount2"} {
		t.Run(method, func(t *testing.T) { testVaultNativeUpdatePreparation(t, method) })
	}
}

func testVaultNativeUpdatePreparation(t *testing.T, method string) {
	cfg := testConfig(t)
	_, stack := localStack(t, cfg)
	oldCache := config.GetCacheDir()
	require.NoError(t, config.SetCacheDir(cfg.CacheDir))
	t.Cleanup(func() { require.NoError(t, config.SetCacheDir(oldCache)) })
	mountDir := filepath.Join(t.TempDir(), "mount")
	require.NoError(t, os.Mkdir(mountDir, 0700))
	_, mountFn := mountlib.ResolveMountMethod(method)
	require.NotNil(t, mountFn)
	mountOpt := mountlib.Opt
	mountOpt.Daemon = false
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.PollInterval = 0
	options.HandleCaching = 0
	options.WriteBack = fs.Duration(time.Hour)
	mount := mountlib.NewMountPoint(mountFn, mountDir, stack, &mountOpt, &options)
	mount.Ctx = context.Background()
	_, err := mount.Mount()
	require.NoError(t, err)
	active := true
	t.Cleanup(func() {
		if active {
			_ = mount.Unmount()
		}
	})
	endpoint, socket := newSyncTestEndpoint(t, mount.VFS, func() {})
	filename := filepath.Join(mountDir, "generated.txt")
	writer, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	data := []byte("generated pending write retained across prepared mount restart")
	_, err = writer.Write(data)
	require.NoError(t, err)

	status, response := syncRequestOverSocket(t, socket, "POST", "/update/prepare", `{"writer_timeout":0.1,"upload_timeout":0}`)
	require.Equal(t, 409, status, string(response))
	require.NoError(t, writer.Sync(), "timeout leaves existing writer usable")
	directory, err := os.Open(mountDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = directory.Close() })
	_, err = directory.ReadDir(-1)
	require.NoError(t, err)

	type result struct {
		status int
		body   []byte
	}
	prepared := make(chan result, 1)
	go func() {
		code, body := syncRequestOverSocket(t, socket, "POST", "/update/prepare", `{"writer_timeout":3,"upload_timeout":0}`)
		prepared <- result{code, body}
	}()
	require.Eventually(t, func() bool {
		handle, openErr := os.OpenFile(filename, os.O_WRONLY, 0600)
		if handle != nil {
			_ = handle.Close()
		}
		return errors.Is(openErr, unix.EBUSY)
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, writer.Close())
	select {
	case response := <-prepared:
		t.Fatalf("preparation returned with an open directory: %d %s", response.status, response.body)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, directory.Close())
	select {
	case response := <-prepared:
		require.Equal(t, 200, response.status, string(response.body))
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not finish after writer closed")
	}
	files, _, err := pending(cfg)
	require.NoError(t, err)
	require.EqualValues(t, 1, files, "preparation preserves closed pending data")
	stopVaultMount(t, mount)
	active = false
	require.NoError(t, endpoint.Close())

	options.WriteBack = fs.Duration(100 * time.Millisecond)
	remount := mountlib.NewMountPoint(mountFn, mountDir, stack, &mountOpt, &options)
	remount.Ctx = context.Background()
	_, err = remount.Mount()
	require.NoError(t, err)
	remounted := true
	t.Cleanup(func() {
		if remounted {
			_ = remount.Unmount()
		}
	})
	got, err := os.ReadFile(filename)
	require.NoError(t, err)
	require.Equal(t, data, got)
	require.Eventually(t, func() bool {
		files, _, err := pending(cfg)
		return err == nil && files == 0
	}, 10*time.Second, 20*time.Millisecond)
	object, err := stack.NewObject(context.Background(), filepath.Base(filename))
	require.NoError(t, err)
	require.Equal(t, data, read(t, object))
	stopVaultMount(t, remount)
	remounted = false
}
