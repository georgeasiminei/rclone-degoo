//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris

package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthFileOSLockHonorsContextCancellation(t *testing.T) {
	authFile := filepath.Join(privateTempDir(t), "auth.json")
	unlock, err := lockAuthFileOS(context.Background(), authFile)
	require.NoError(t, err)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = lockAuthFileOS(ctx, authFile)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), time.Second)
}

func TestAuthFileOSLockIsPrivateAndRejectsUnsafeFiles(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	lockPath := authFile + ".lock"
	unlock, err := lockAuthFileOS(context.Background(), authFile)
	require.NoError(t, err)
	info, err := os.Lstat(lockPath)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	unlock()

	require.NoError(t, os.Chmod(lockPath, 0o644))
	_, err = lockAuthFileOS(context.Background(), authFile)
	require.ErrorContains(t, err, "owner-only regular file")
	require.NoError(t, os.Remove(lockPath))

	target := filepath.Join(dir, "target")
	require.NoError(t, WritePrivateFile(target, []byte("private")))
	require.NoError(t, os.Symlink(target, lockPath))
	_, err = lockAuthFileOS(context.Background(), authFile)
	require.ErrorContains(t, err, "auth lock")
}
