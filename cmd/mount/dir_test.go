//go:build linux

package mount

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"github.com/rclone/rclone/vfs"
	"github.com/stretchr/testify/require"
)

func TestDirOpenTracksHandleForMaintenance(t *testing.T) {
	_, dir := newXattrTestNodes(t, false)
	ctx := context.Background()

	handle, err := dir.Open(ctx, &fuse.OpenRequest{}, &fuse.OpenResponse{})
	require.NoError(t, err)
	readDir, ok := handle.(fusefs.HandleReadDirAller)
	require.True(t, ok)
	releaser, ok := handle.(fusefs.HandleReleaser)
	require.True(t, ok)

	first, err := readDir.ReadDirAll(ctx)
	require.NoError(t, err)
	second, err := readDir.ReadDirAll(ctx)
	require.NoError(t, err)
	require.Equal(t, first, second, "rewinding a FUSE directory handle should reread entries")
	require.Contains(t, direntNames(first), "status-file")

	prepared := make(chan error, 1)
	go func() {
		release, err := dir.VFS().PrepareMaintenance(ctx)
		if release != nil {
			release()
		}
		prepared <- err
	}()

	require.Eventually(t, func() bool {
		opened, err := dir.VFS().Open("status-file")
		if errors.Is(err, vfs.EBUSY) {
			return true
		}
		if err == nil {
			_ = opened.Release()
		}
		return false
	}, time.Second, 10*time.Millisecond)
	select {
	case err := <-prepared:
		t.Fatalf("maintenance completed with an open FUSE directory handle: %v", err)
	default:
	}

	require.NoError(t, releaser.Release(ctx, &fuse.ReleaseRequest{}))
	select {
	case err := <-prepared:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not complete after the FUSE directory handle was released")
	}
	require.NoError(t, releaser.Release(ctx, &fuse.ReleaseRequest{}), "release is idempotent")
	_, err = readDir.ReadDirAll(ctx)
	require.ErrorIs(t, err, syscall.EBADF)
}

func direntNames(dirents []fuse.Dirent) []string {
	names := make([]string, len(dirents))
	for i, dirent := range dirents {
		names[i] = dirent.Name
	}
	return names
}
