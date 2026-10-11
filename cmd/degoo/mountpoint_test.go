//go:build linux

package degoo

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

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

func TestMountpointImmutableWithoutOwnerWriteAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a directory without write permission")
	}
	dir := filepath.Join(t.TempDir(), "mount")
	guard, err := protectMountpoint(dir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, guard.Close())
		require.NoError(t, os.Chmod(dir, 0700))
	})
	guard.immutable = true
	require.ErrorContains(t, guard.prepare(), "not writable by the current user")
	require.ErrorContains(t, guard.prepare(), "immutable flag may remain set")
}

func TestMountpointImmutableWithOwnerWriteAccess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mount")
	guard, err := protectMountpoint(dir)
	require.NoError(t, err)
	require.NoError(t, guard.file.Chmod(0700))
	guard.immutable = true

	require.NoError(t, guard.prepare())
	require.NoError(t, guard.protect())
	require.NoError(t, guard.Close())
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
}
