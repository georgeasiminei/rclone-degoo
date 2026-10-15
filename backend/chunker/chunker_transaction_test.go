package chunker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/stretchr/testify/require"
)

func TestNoRenameFailedPublicationPreservesPriorChunks(t *testing.T) {
	ctx := context.Background()
	base, err := fs.NewFs(ctx, t.TempDir())
	require.NoError(t, err)
	f := &Fs{base: base, opt: Options{ChunkSize: 128, StartFrom: 1, NameFormat: "*.rclone_chunk.###", MetaFormat: "simplejson", HashType: "sha1all", Transactions: "norename"}}
	require.NoError(t, f.configure(f.opt.NameFormat, f.opt.MetaFormat, f.opt.HashType, f.opt.Transactions))
	f.features = (&fs.Features{}).Fill(ctx, f).Mask(ctx, base)
	old := bytes.Repeat([]byte("prior complete contents "), 20)
	info := object.NewStaticObjectInfo("file", time.Now(), int64(len(old)), true, nil, base)
	_, err = f.Put(ctx, bytes.NewReader(old), info)
	require.NoError(t, err)
	before, err := base.List(ctx, "")
	require.NoError(t, err)
	updated := bytes.Repeat([]byte("replacement contents "), 30)
	info = object.NewStaticObjectInfo("file", time.Now(), int64(len(updated)), true, nil, base)
	failed := errors.New("metadata publication failed")
	put := func(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
		if src.Remote() == "file" {
			return nil, failed
		}
		return base.Put(ctx, in, src, options...)
	}
	_, err = f.put(ctx, bytes.NewReader(updated), info, "file", nil, put, "put", nil)
	require.ErrorIs(t, err, failed)
	obj, err := f.NewObject(ctx, "file")
	require.NoError(t, err)
	r, err := obj.Open(ctx)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.Equal(t, old, got)
	after, err := base.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, after, len(before), "failed transaction chunks must be removed")

	_, err = f.Put(ctx, bytes.NewReader(updated), info)
	require.NoError(t, err)
	obj, err = f.NewObject(ctx, "file")
	require.NoError(t, err)
	r, err = obj.Open(ctx)
	require.NoError(t, err)
	got, err = io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.Equal(t, updated, got)
	for _, entry := range before {
		if entry.Remote() != "file" {
			_, err := base.NewObject(ctx, entry.Remote())
			require.ErrorIs(t, err, fs.ErrorObjectNotFound, "committed replacement must retire old chunks")
		}
	}
}

func TestNoRenameMissingAllChunksDoesNotReturnMetadata(t *testing.T) {
	ctx := context.Background()
	base, err := fs.NewFs(ctx, t.TempDir())
	require.NoError(t, err)
	f := &Fs{base: base, opt: Options{ChunkSize: 128, StartFrom: 1, NameFormat: "*.rclone_chunk.###", MetaFormat: "simplejson", HashType: "sha1all", Transactions: "norename", FailHard: true}}
	require.NoError(t, f.configure(f.opt.NameFormat, f.opt.MetaFormat, f.opt.HashType, f.opt.Transactions))
	f.features = (&fs.Features{}).Fill(ctx, f).Mask(ctx, base)
	data := []byte("generated single chunk")
	info := object.NewStaticObjectInfo("file", time.Now(), int64(len(data)), true, nil, base)
	obj, err := f.Put(ctx, bytes.NewReader(data), info)
	require.NoError(t, err)
	for _, chunk := range obj.(*Object).chunks {
		require.NoError(t, chunk.Remove(ctx))
	}
	obj, err = f.NewObject(ctx, "file")
	if err == nil {
		r, openErr := obj.Open(ctx)
		if openErr == nil {
			_, err = io.ReadAll(r)
			require.NoError(t, r.Close())
		} else {
			err = openErr
		}
	}
	require.Error(t, err, "an orphan manifest must not be served as file contents")
	_, err = f.List(ctx, "")
	require.Error(t, err, "fail_hard must reject orphan manifests during listing")
}
