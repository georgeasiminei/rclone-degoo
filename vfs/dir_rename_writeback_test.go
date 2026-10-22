package vfs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/vfs/vfscache/writeback"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

// dirRenameRaceFs disables remote move operations and can pause a listing
// after taking its snapshot, as a slow fallback directory move might.
type dirRenameRaceFs struct {
	fs.Fs
	features        fs.Features
	source          string
	armed           bool
	listEntered     chan struct{}
	continueListing chan struct{}
	listOnce        sync.Once
	releaseOnce     sync.Once
	blockPath       string
	putStarted      chan struct{}
	continuePut     chan struct{}
	putOnce         sync.Once
	releasePutOnce  sync.Once
	putPaths        chan string
}

func (f *dirRenameRaceFs) releaseList() {
	f.releaseOnce.Do(func() { close(f.continueListing) })
}

func (f *dirRenameRaceFs) releasePut() {
	if f.continuePut != nil {
		f.releasePutOnce.Do(func() { close(f.continuePut) })
	}
}

func (f *dirRenameRaceFs) Features() *fs.Features {
	return &f.features
}

func (f *dirRenameRaceFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := f.Fs.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	if f.armed && dir == f.source {
		f.listOnce.Do(func() {
			close(f.listEntered)
			select {
			case <-f.continueListing:
			case <-ctx.Done():
			}
		})
	}
	return entries, nil
}

func (f *dirRenameRaceFs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	if f.blockPath != "" && src.Remote() == f.blockPath {
		f.putOnce.Do(func() {
			close(f.putStarted)
			select {
			case <-f.continuePut:
			case <-ctx.Done():
			}
		})
	}
	obj, err := f.Fs.Put(ctx, in, src, options...)
	if err == nil {
		f.putPaths <- src.Remote()
	}
	return obj, err
}

func newDirRenameRaceFs(t *testing.T) (*dirRenameRaceFs, string) {
	t.Helper()
	rootPath := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(rootPath, "source"), 0o700))
	baseFs, err := local.NewFs(context.Background(), "dir-rename-writeback-test", rootPath, configmap.Simple{})
	require.NoError(t, err)
	features := *baseFs.Features()
	features.DirMove = nil
	features.Move = nil
	features.ListR = nil
	features.PutStream = nil
	features.Copy = func(context.Context, fs.Object, string) (fs.Object, error) {
		return nil, fs.ErrorCantCopy
	}
	return &dirRenameRaceFs{
		Fs:              baseFs,
		features:        features,
		source:          "source",
		listEntered:     make(chan struct{}),
		continueListing: make(chan struct{}),
		putPaths:        make(chan string, 4),
	}, rootPath
}

func TestDirRenamePausesQueuedDescendantWriteback(t *testing.T) {
	ctx := context.Background()
	raceFs, rootPath := newDirRenameRaceFs(t)

	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeWrites
	opt.WriteBack = fs.Duration(time.Hour)
	vfs := New(ctx, raceFs, &opt)
	t.Cleanup(func() {
		raceFs.releasePut()
		raceFs.releaseList()
		if queue, ok := vfs.cache.Queue()["queue"].([]writeback.QueueInfo); ok {
			for _, item := range queue {
				if !item.Uploading {
					_ = vfs.cache.QueueSetExpiry(item.ID, time.Now().Add(-time.Second), 0)
				}
			}
		}
		vfs.WaitForWriters(5 * time.Second)
		require.NoError(t, vfs.CleanUp())
		vfs.Shutdown()
	})

	_, err := vfs.Stat("source")
	require.NoError(t, err)

	// Borg's owner marker is an empty file. Close queues it in the VFS cache.
	h, err := vfs.Create("source/lock.exclusive")
	require.NoError(t, err)
	require.NoError(t, h.Close())
	queue, ok := vfs.cache.Queue()["queue"].([]writeback.QueueInfo)
	require.True(t, ok)
	require.Len(t, queue, 1)

	raceFs.armed = true
	renameDone := make(chan error, 1)
	go func() {
		renameDone <- vfs.Rename("source", "destination")
	}()
	select {
	case <-raceFs.listEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("fallback directory rename did not list its source")
	}

	// Expire the queued write only after the fallback has captured its source
	// listing. Without a prefix pause this deterministically uploads the marker
	// into the source directory before the fallback tries to remove it.
	require.NoError(t, vfs.cache.QueueSetExpiry(queue[0].ID, time.Now().Add(-time.Second), 0))
	uploadedDuringListing := false
	select {
	case got := <-raceFs.putPaths:
		uploadedDuringListing = true
		require.Equal(t, "source/lock.exclusive", got)
	case <-time.After(250 * time.Millisecond):
	}
	raceFs.releaseList()

	select {
	case err := <-renameDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("directory rename did not finish")
	}
	require.False(t, uploadedDuringListing, "a queued descendant upload escaped during the directory move")
	queue, ok = vfs.cache.Queue()["queue"].([]writeback.QueueInfo)
	require.True(t, ok)
	require.Len(t, queue, 1, "the pending marker must remain queued through the rename")
	require.NoError(t, vfs.cache.QueueSetExpiry(queue[0].ID, time.Now().Add(-time.Second), 0))
	select {
	case got := <-raceFs.putPaths:
		require.Equal(t, "destination/lock.exclusive", got)
	case <-time.After(5 * time.Second):
		t.Fatal("queued marker was not uploaded under its renamed path")
	}
	vfs.WaitForWriters(5 * time.Second)
	_, err = os.Stat(filepath.Join(rootPath, "destination", "lock.exclusive"))
	require.NoError(t, err)

	// Complete the marker lifecycle: unlink it, remove the now-empty directory,
	// then make sure no delayed writeback recreates either path.
	require.NoError(t, vfs.Remove("destination/lock.exclusive"))
	require.NoError(t, vfs.Remove("destination"))
	vfs.WaitForWriters(5 * time.Second)
	time.Sleep(100 * time.Millisecond)
	_, err = os.Stat(filepath.Join(rootPath, "destination"))
	require.ErrorIs(t, err, os.ErrNotExist)
	queue, ok = vfs.cache.Queue()["queue"].([]writeback.QueueInfo)
	require.True(t, ok)
	require.Empty(t, queue, "unlinked marker must not be re-uploaded later")
}

func TestDirRenameWaitsForActiveDescendantWriteback(t *testing.T) {
	ctx := context.Background()
	raceFs, rootPath := newDirRenameRaceFs(t)
	raceFs.blockPath = "source/lock.exclusive"
	raceFs.putStarted = make(chan struct{})
	raceFs.continuePut = make(chan struct{})
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeWrites
	opt.WriteBack = fs.Duration(20 * time.Millisecond)
	vfs := New(ctx, raceFs, &opt)
	t.Cleanup(func() {
		raceFs.releasePut()
		raceFs.releaseList()
		if queue, ok := vfs.cache.Queue()["queue"].([]writeback.QueueInfo); ok {
			for _, item := range queue {
				if !item.Uploading {
					_ = vfs.cache.QueueSetExpiry(item.ID, time.Now().Add(-time.Second), 0)
				}
			}
		}
		vfs.WaitForWriters(5 * time.Second)
		require.NoError(t, vfs.CleanUp())
		vfs.Shutdown()
	})

	_, err := vfs.Stat("source")
	require.NoError(t, err)
	h, err := vfs.Create("source/lock.exclusive")
	require.NoError(t, err)
	require.NoError(t, h.Close())
	select {
	case <-raceFs.putStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("descendant writeback did not start")
	}

	raceFs.armed = true
	renameDone := make(chan error, 1)
	go func() {
		renameDone <- vfs.Rename("source", "destination")
	}()
	select {
	case <-raceFs.listEntered:
		t.Fatal("directory snapshot started before the active descendant upload finished")
	case <-time.After(100 * time.Millisecond):
	}

	// Completing this upload runs the VFS store callback, which updates the
	// source directory's entry map before the pause may return.
	raceFs.releasePut()
	select {
	case <-raceFs.listEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("directory snapshot did not start after the active upload finished")
	}
	raceFs.releaseList()
	select {
	case err := <-renameDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("directory rename did not finish after the active upload completed")
	}
	vfs.WaitForWriters(5 * time.Second)
	_, err = os.Stat(filepath.Join(rootPath, "destination", "lock.exclusive"))
	require.NoError(t, err)
}
