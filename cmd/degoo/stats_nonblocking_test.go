//go:build linux

package degoo

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/require"
)

type statsGatedListingFs struct {
	fs.Fs
	features        fs.Features
	blockedDir      string
	armed           atomic.Bool
	entered         chan struct{}
	continueListing chan struct{}
	enterOnce       sync.Once
	releaseOnce     sync.Once
}

func (f *statsGatedListingFs) Features() *fs.Features {
	return &f.features
}

func (f *statsGatedListingFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	if f.armed.Load() && dir == f.blockedDir {
		f.enterOnce.Do(func() {
			close(f.entered)
			select {
			case <-f.continueListing:
			case <-ctx.Done():
			}
		})
	}
	return f.Fs.List(ctx, dir)
}

func (f *statsGatedListingFs) releaseList() {
	f.releaseOnce.Do(func() { close(f.continueListing) })
}

func TestRuntimeStatsDoesNotWaitForRemoteDirectoryListing(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("requires a local test remote")
	}
	run := fstest.NewRunIndividual(t)
	ctx := context.Background()
	const blockedDir = "slow-directory"
	require.NoError(t, run.Fremote.Mkdir(ctx, blockedDir))
	features := *run.Fremote.Features()
	features.ListR = nil
	remote := &statsGatedListingFs{
		Fs:              run.Fremote,
		features:        features,
		blockedDir:      blockedDir,
		entered:         make(chan struct{}),
		continueListing: make(chan struct{}),
	}
	mounted := newSyncTestVFS(t, remote, true)
	t.Cleanup(remote.releaseList)
	_, err := mounted.Stat(blockedDir)
	require.NoError(t, err)
	remote.armed.Store(true)

	listingDone := make(chan error, 1)
	go func() {
		_, err := mounted.ReadDir(blockedDir)
		listingDone <- err
	}()
	select {
	case <-remote.entered:
	case err := <-listingDone:
		t.Fatalf("directory listing returned before reaching the gate: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("directory listing did not reach the gate")
	}
	statsDone := make(chan error, 1)
	go func() {
		_, err := collectRuntimeStats(ctx, mounted, time.Now())
		statsDone <- err
	}()
	select {
	case err := <-statsDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		remote.releaseList()
		require.NoError(t, <-listingDone)
		t.Fatal("local runtime stats waited for the gated remote directory listing")
	}

	remote.releaseList()
	select {
	case err := <-listingDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("directory listing did not finish after the gate opened")
	}
	require.Equal(t, mounted.DiskCacheStats(), mounted.Stats()["diskCache"])
}
