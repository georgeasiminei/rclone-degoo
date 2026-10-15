//go:build linux || (darwin && amd64)

package mount2

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

// TestDirStreamSeekdir checks that Seekdir repositions the snapshot directory
// stream to an arbitrary offset. A kernel-NFS-exported mount needs this for
// readdir continuation: the stateless server opens a fresh handle and seeks to
// the last returned cookie on every batch. Offset 0 must still reset to the
// start (the rewind / re-read case) and offsets at or past the end must clamp
// to EOF.
func TestDirStreamSeekdir(t *testing.T) {
	ctx := context.Background()
	// 3 real entries plus the synthesized "." and ".." => 5 entries total,
	// occupying internal indices 0..4 (go-fuse offsets 1..5). HasNext is true
	// while i < len(nodes)+2, i.e. i < 5.
	ds := &dirStream{nodes: make([]os.FileInfo, 3)}

	for _, tc := range []struct {
		off      uint64
		wantI    int
		wantNext bool
	}{
		{0, 0, true},    // rewind to start (the re-read case)
		{2, 2, true},    // resume at the first real entry
		{4, 4, true},    // last entry
		{5, 5, false},   // exactly at end => EOF
		{99, 99, false}, // past end => clamped to EOF via HasNext
	} {
		if errno := ds.Seekdir(ctx, tc.off); errno != 0 {
			t.Fatalf("Seekdir(%d) returned errno %v, want 0", tc.off, errno)
		}
		if ds.i != tc.wantI {
			t.Errorf("Seekdir(%d): i = %d, want %d", tc.off, ds.i, tc.wantI)
		}
		if got := ds.HasNext(); got != tc.wantNext {
			t.Errorf("Seekdir(%d): HasNext() = %v, want %v", tc.off, got, tc.wantNext)
		}
	}
}

func TestOpendirHandleTracksMaintenanceUntilReleasedir(t *testing.T) {
	ctx := context.Background()
	r := fstest.NewRunIndividual(t)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeOff
	vfsys := vfs.New(ctx, r.Fremote, &opt)
	t.Cleanup(vfsys.Shutdown)
	root, err := vfsys.Root()
	require.NoError(t, err)
	node := &Node{node: root, fsys: NewFS(vfsys, &mountlib.Options{})}

	rawHandle, flags, errno := node.OpendirHandle(ctx, uint32(os.O_RDONLY))
	require.Zero(t, errno)
	require.Zero(t, flags)
	handle, ok := rawHandle.(*dirFileHandle)
	require.True(t, ok)

	entry, errno := handle.Readdirent(ctx)
	require.Zero(t, errno)
	require.NotNil(t, entry)
	require.Equal(t, ".", entry.Name)
	require.Zero(t, handle.Seekdir(ctx, 0))
	entry, errno = handle.Readdirent(ctx)
	require.Zero(t, errno)
	require.NotNil(t, entry)
	require.Equal(t, ".", entry.Name, "seeking to offset zero must rewind this open directory FD")

	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = vfsys.PrepareMaintenance(deadline)
	require.ErrorIs(t, err, context.DeadlineExceeded, "maintenance must wait for the open directory FD")

	handle.Releasedir(ctx, 0)
	release, err := vfsys.PrepareMaintenance(ctx)
	require.NoError(t, err, "maintenance should proceed after releasedir closes the tracked VFS handle")
	_, _, errno = node.OpendirHandle(ctx, uint32(os.O_RDONLY))
	require.Equal(t, syscall.EBUSY, errno, "maintenance must reject new directory opens")
	release()

	opened, _, errno := node.OpendirHandle(ctx, uint32(os.O_RDONLY))
	require.Zero(t, errno, "directory opens should resume after maintenance is released")
	opened.(*dirFileHandle).Releasedir(ctx, 0)
}
