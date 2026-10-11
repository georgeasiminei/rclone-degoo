//go:build linux

package degoo

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

var degooImmutableTest = flag.Bool("degoo-immutable-test", false, "test a real immutable directory using sudo -n chattr on a generated fixture")

func TestMountpointProtection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mount")
	require.NoError(t, os.Mkdir(dir, 0700))
	guard, err := protectMountpoint(dir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, guard.Close())
		require.NoError(t, os.Chmod(dir, 0700))
	})
	assertMode := func(mode os.FileMode) {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.Equal(t, mode, info.Mode().Perm())
	}
	assertMode(0500)
	require.NoError(t, guard.prepare())
	assertMode(0700)
	require.NoError(t, guard.protect())
	assertMode(0500)
}

func TestMountpointProtectionPreservesLocalFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mount")
	require.NoError(t, os.Mkdir(dir, 0700))
	filename := filepath.Join(dir, "b.txt")
	require.NoError(t, os.WriteFile(filename, []byte("generated local data"), 0600))
	guard, err := protectMountpoint(dir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, guard.Close())
		require.NoError(t, os.Chmod(dir, 0700))
	})
	require.ErrorContains(t, guard.prepare(), "ordinary local files")
	data, err := os.ReadFile(filename)
	require.NoError(t, err)
	require.Equal(t, "generated local data", string(data))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0500), info.Mode().Perm())
}

func TestMountpointProtectionRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "mount")
	require.NoError(t, os.Symlink(dir, link))
	_, err := protectMountpoint(link)
	require.Error(t, err)
}

func TestMountpointRealImmutableFlagRetained(t *testing.T) {
	if !*degooImmutableTest {
		t.Skip("pass -degoo-immutable-test to test a real filesystem immutable flag")
	}
	dir := filepath.Join(t.TempDir(), "mount")
	require.NoError(t, os.Mkdir(dir, 0700))
	output, err := exec.Command("sudo", "-n", "chattr", "+i", "--", dir).CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Cleanup(func() {
		output, err := exec.Command("sudo", "-n", "chattr", "-i", "--", dir).CombinedOutput()
		require.NoError(t, err, "%s", output)
	})
	guard, err := protectMountpoint(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = guard.file.Close() })
	require.True(t, guard.immutable)
	require.ErrorIs(t, unix.Access(dir, unix.W_OK), unix.EPERM)
	require.NoError(t, guard.prepare())
	require.NoError(t, guard.protect())
	flags, err := unix.IoctlGetInt(int(guard.file.Fd()), unix.FS_IOC_GETFLAGS)
	require.NoError(t, err)
	require.NotZero(t, flags&fsImmutableFlag)
	require.NoError(t, guard.Close())
	reopened, err := protectMountpoint(dir)
	// Verify writes are still blocked after the retained descriptor is closed.
	require.NoError(t, err)
	require.True(t, reopened.immutable)
	require.NoError(t, reopened.Close())
	require.ErrorIs(t, os.WriteFile(filepath.Join(dir, "local-only"), []byte("generated"), 0600), unix.EPERM)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
}
