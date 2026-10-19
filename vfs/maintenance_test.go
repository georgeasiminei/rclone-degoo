package vfs

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs/vfscache/writeback"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareMaintenanceWaitsForHandlesAndBlocksAdmissions(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeWrites
	opt.WriteBack = fs.Duration(time.Hour)
	opt.CachePollInterval = 0
	opt.HandleCaching = 0
	r, vfs := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "file", "initial", t1)

	node, err := vfs.Stat("file")
	require.NoError(t, err)
	h, err := node.(*File).Open(os.O_WRONLY | os.O_TRUNC)
	require.NoError(t, err)
	_, err = h.Write([]byte("updated"))
	require.NoError(t, err)

	type prepareResult struct {
		release func()
		err     error
	}
	prepared := make(chan prepareResult, 1)
	go func() {
		release, err := vfs.PrepareMaintenance(context.Background())
		prepared <- prepareResult{release: release, err: err}
	}()

	require.Eventually(t, func() bool {
		opened, err := vfs.Open("file")
		if errors.Is(err, EBUSY) {
			return true
		}
		if err == nil {
			_ = opened.Close()
		}
		return false
	}, time.Second, 10*time.Millisecond)
	assert.ErrorIs(t, vfs.Mkdir("blocked", 0o755), EBUSY)
	assert.ErrorIs(t, vfs.Remove("file"), EBUSY)
	_, err = node.(*File).Open(os.O_RDONLY)
	assert.ErrorIs(t, err, EBUSY)
	assert.ErrorIs(t, node.(*File).Remove(), EBUSY)
	root, err := vfs.Root()
	require.NoError(t, err)
	_, err = root.Create("direct-blocked", os.O_CREATE|os.O_WRONLY)
	assert.ErrorIs(t, err, EBUSY)
	_, err = root.Mkdir("direct-blocked")
	assert.ErrorIs(t, err, EBUSY)
	assert.ErrorIs(t, root.RemoveName("file"), EBUSY)
	assert.ErrorIs(t, root.Rename("file", "renamed", root), EBUSY)
	select {
	case <-prepared:
		t.Fatal("maintenance returned while a file handle was open")
	default:
	}

	_, err = h.Write([]byte(" after"))
	require.NoError(t, err)
	require.NoError(t, h.Close())

	var result prepareResult
	select {
	case result = <-prepared:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not complete after the handle closed")
	}
	require.NoError(t, result.err)
	require.NotNil(t, result.release)
	assert.ErrorIs(t, func() error { _, err := vfs.Open("file"); return err }(), EBUSY)
	assert.ErrorIs(t, vfs.Mkdir("still-blocked", 0o755), EBUSY)

	result.release()
	result.release()
	opened, err := vfs.Open("file")
	require.NoError(t, err)
	require.NoError(t, opened.Close())

	queue := vfs.cache.Queue()["queue"].([]writeback.QueueInfo)
	for _, item := range queue {
		require.NoError(t, vfs.cache.QueueSetExpiry(item.ID, time.Now(), 0))
	}
}

func TestPrepareMaintenanceTimeoutReleasesBarrier(t *testing.T) {
	r, vfs := newTestVFS(t)
	r.WriteObject(context.Background(), "file", "contents", t1)
	h, err := vfs.Open("file")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	release, err := vfs.PrepareMaintenance(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, release)

	opened, err := vfs.Open("file")
	require.NoError(t, err, "a timed-out preparation must release the barrier")
	require.NoError(t, opened.Close())
	require.NoError(t, h.Release(), "releasing an unopened read handle must drain maintenance tracking")
}

func TestPrepareMaintenanceWaitsForDirectoryHandle(t *testing.T) {
	_, vfs := newTestVFS(t)
	root, err := vfs.Root()
	require.NoError(t, err)
	h, err := root.Open(os.O_RDONLY)
	require.NoError(t, err)

	prepared := make(chan error, 1)
	go func() {
		release, err := vfs.PrepareMaintenance(context.Background())
		if release != nil {
			release()
		}
		prepared <- err
	}()

	require.Eventually(t, func() bool {
		opened, err := vfs.Open("/")
		if errors.Is(err, EBUSY) {
			return true
		}
		if err == nil {
			_ = opened.Close()
		}
		return false
	}, time.Second, 10*time.Millisecond)
	select {
	case <-prepared:
		t.Fatal("maintenance returned while a directory handle was open")
	default:
	}

	require.NoError(t, h.Release())
	select {
	case err := <-prepared:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not complete after the directory handle was released")
	}
}

func TestHasPendingWritesWithoutCache(t *testing.T) {
	_, vfs := newTestVFS(t)
	pending, err := vfs.HasPendingWrites(context.Background())
	require.NoError(t, err)
	assert.False(t, pending)
}
