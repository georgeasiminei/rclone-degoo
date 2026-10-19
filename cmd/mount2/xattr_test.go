//go:build linux || (darwin && amd64)

package mount2

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

func newXattrTestNodes(t *testing.T, uploadStatus bool) (*Node, *Node) {
	t.Helper()
	ctx := context.Background()
	r := fstest.NewRunIndividual(t)
	r.WriteObject(ctx, "status-file", "contents", time.Unix(123, 0))

	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeOff
	opt.UploadStatus = uploadStatus
	vfsys := vfs.New(ctx, r.Fremote, &opt)
	t.Cleanup(vfsys.Shutdown)

	fileNode, err := vfsys.Stat("status-file")
	require.NoError(t, err)
	root, err := vfsys.Root()
	require.NoError(t, err)
	fsys := NewFS(vfsys, &mountlib.Options{})
	return &Node{node: fileNode, fsys: fsys}, &Node{node: root, fsys: fsys}
}

func TestNodeUploadStatusXattr(t *testing.T) {
	file, dir := newXattrTestNodes(t, true)
	ctx := context.Background()

	size, errno := file.Getxattr(ctx, vfs.UploadStatusXattr, nil)
	require.Zero(t, errno)
	require.Equal(t, uint32(len("uploaded")), size)
	value := make([]byte, size)
	got, errno := file.Getxattr(ctx, vfs.UploadStatusXattr, value)
	require.Zero(t, errno)
	require.Equal(t, size, got)
	require.Equal(t, "uploaded", string(value))

	_, errno = file.Getxattr(ctx, "user.unknown", nil)
	require.Equal(t, syscall.ENODATA, errno)
	require.Equal(t, syscall.EPERM, file.Setxattr(ctx, vfs.UploadStatusXattr, []byte("changed"), 0))
	require.Equal(t, syscall.EPERM, file.Removexattr(ctx, vfs.UploadStatusXattr))

	listSize, errno := file.Listxattr(ctx, nil)
	require.Zero(t, errno)
	wantList := vfs.UploadStatusXattr + "\x00"
	require.Equal(t, uint32(len(wantList)), listSize)
	list := make([]byte, listSize)
	got, errno = file.Listxattr(ctx, list)
	require.Zero(t, errno)
	require.Equal(t, listSize, got)
	require.Equal(t, wantList, string(list))

	_, errno = dir.Getxattr(ctx, vfs.UploadStatusXattr, nil)
	require.Equal(t, syscall.ENODATA, errno)
	dirListSize, errno := dir.Listxattr(ctx, nil)
	require.Zero(t, errno)
	require.Zero(t, dirListSize)
}

func TestNodeXattrDisabled(t *testing.T) {
	file, dir := newXattrTestNodes(t, false)
	ctx := context.Background()

	_, errno := file.Getxattr(ctx, vfs.UploadStatusXattr, nil)
	require.Equal(t, syscall.ENOSYS, errno)
	_, errno = file.Listxattr(ctx, nil)
	require.Equal(t, syscall.ENOSYS, errno)
	require.Equal(t, syscall.ENOSYS, file.Setxattr(ctx, vfs.UploadStatusXattr, nil, 0))
	require.Equal(t, syscall.ENOSYS, file.Removexattr(ctx, vfs.UploadStatusXattr))

	_, errno = dir.Getxattr(ctx, vfs.UploadStatusXattr, nil)
	require.Equal(t, syscall.ENOSYS, errno)
	_, errno = dir.Listxattr(ctx, nil)
	require.Equal(t, syscall.ENOSYS, errno)
}

func TestCopyXattrSizeProbeAndRange(t *testing.T) {
	value := "user.rclone.upload-status\x00"
	wantSize := uint32(len(value))

	size, errno := copyXattr(nil, value)
	require.Zero(t, errno)
	require.Equal(t, wantSize, size)

	dest := make([]byte, len(value)-1)
	size, errno = copyXattr(dest, value)
	require.Equal(t, syscall.ERANGE, errno)
	require.Equal(t, wantSize, size)

	dest = make([]byte, len(value))
	size, errno = copyXattr(dest, value)
	require.Zero(t, errno)
	require.Equal(t, wantSize, size)
	require.Equal(t, value, string(dest))
}
