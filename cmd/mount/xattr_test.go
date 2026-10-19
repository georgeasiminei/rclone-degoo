//go:build linux

package mount

import (
	"context"
	"syscall"
	"testing"
	"time"

	"bazil.org/fuse"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

func newXattrTestNodes(t *testing.T, uploadStatus bool) (*File, *Dir) {
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
	return &File{File: fileNode.(*vfs.File), fsys: fsys}, &Dir{Dir: root, fsys: fsys}
}

func TestFileUploadStatusXattr(t *testing.T) {
	file, dir := newXattrTestNodes(t, true)
	ctx := context.Background()

	resp := new(fuse.GetxattrResponse)
	err := file.Getxattr(ctx, &fuse.GetxattrRequest{Name: vfs.UploadStatusXattr}, resp)
	require.NoError(t, err)
	require.Equal(t, []byte("uploaded"), resp.Xattr)

	err = file.Getxattr(ctx, &fuse.GetxattrRequest{Name: "user.unknown"}, new(fuse.GetxattrResponse))
	require.ErrorIs(t, err, fuse.ErrNoXattr)

	list := new(fuse.ListxattrResponse)
	err = file.Listxattr(ctx, new(fuse.ListxattrRequest), list)
	require.NoError(t, err)
	require.Equal(t, vfs.UploadStatusXattr+"\x00", string(list.Xattr))

	err = file.Setxattr(ctx, &fuse.SetxattrRequest{Name: vfs.UploadStatusXattr, Xattr: []byte("changed")})
	require.ErrorIs(t, err, syscall.EPERM)
	err = file.Removexattr(ctx, &fuse.RemovexattrRequest{Name: vfs.UploadStatusXattr})
	require.ErrorIs(t, err, syscall.EPERM)

	err = dir.Getxattr(ctx, &fuse.GetxattrRequest{Name: vfs.UploadStatusXattr}, new(fuse.GetxattrResponse))
	require.ErrorIs(t, err, fuse.ErrNoXattr)
	dirList := new(fuse.ListxattrResponse)
	err = dir.Listxattr(ctx, new(fuse.ListxattrRequest), dirList)
	require.NoError(t, err)
	require.Empty(t, dirList.Xattr)
}

func TestXattrDisabled(t *testing.T) {
	file, dir := newXattrTestNodes(t, false)
	ctx := context.Background()

	err := file.Getxattr(ctx, &fuse.GetxattrRequest{Name: vfs.UploadStatusXattr}, new(fuse.GetxattrResponse))
	require.ErrorIs(t, err, syscall.ENOSYS)
	err = file.Listxattr(ctx, new(fuse.ListxattrRequest), new(fuse.ListxattrResponse))
	require.ErrorIs(t, err, syscall.ENOSYS)

	err = dir.Getxattr(ctx, &fuse.GetxattrRequest{Name: vfs.UploadStatusXattr}, new(fuse.GetxattrResponse))
	require.ErrorIs(t, err, syscall.ENOSYS)
	err = dir.Listxattr(ctx, new(fuse.ListxattrRequest), new(fuse.ListxattrResponse))
	require.ErrorIs(t, err, syscall.ENOSYS)
}
