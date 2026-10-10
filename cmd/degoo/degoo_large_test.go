//go:build linux

package degoo

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/stretchr/testify/require"
)

const largeTestChunkSize int64 = 1 << 30

var degooLargeTest = flag.Bool("degoo-large-test", false, "run the opt-in 21 GiB Degoo vault test")

func TestVaultLargeLogicalFile(t *testing.T) {
	if !*degooLargeTest {
		t.Skip("pass -degoo-large-test to run the 21 GiB local vault test")
	}

	const logicalSize int64 = 21 * largeTestChunkSize
	ctx := t.Context()
	cfg := testConfig(t)
	cfg.ChunkSize = fs.SizeSuffix(largeTestChunkSize)
	cfg.MaxObjectSize = fs.SizeSuffix(19 * largeTestChunkSize)
	require.NoError(t, cfg.validate())
	base, stack := localStack(t, cfg)

	const name = "generated-large-dir/generated-large-file.bin"
	sourceHash := sha256.New()
	source := io.TeeReader(io.LimitReader(zeroReader{}, logicalSize), sourceHash)
	info := object.NewStaticObjectInfo(name, time.Now(), logicalSize, true, nil, stack)
	stored, err := stack.Put(ctx, source, info)
	require.NoError(t, err)
	require.Equal(t, logicalSize, stored.Size())

	rootEntries, err := base.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, rootEntries, 1)
	remoteDir := rootEntries[0].Remote()
	require.NotContains(t, remoteDir, name)
	require.NotContains(t, remoteDir, "rclone_chunk")
	objects, err := base.List(ctx, remoteDir)
	require.NoError(t, err)
	require.Len(t, objects, int(logicalSize/largeTestChunkSize)+1)
	for _, entry := range objects {
		require.NotContains(t, entry.Remote(), name)
		require.NotContains(t, entry.Remote(), "rclone_chunk")
		require.Greater(t, entry.Size(), int64(0))
		require.LessOrEqual(t, entry.Size(), int64(cfg.MaxObjectSize))
		obj, ok := entry.(fs.Object)
		require.True(t, ok)
		reader, err := obj.Open(ctx)
		require.NoError(t, err)
		prefix := make([]byte, len("RCLONE"))
		_, err = io.ReadFull(reader, prefix)
		require.NoError(t, err)
		require.Equal(t, "RCLONE", string(prefix), "chunk data and encrypted manifest have the crypt header")
		require.NoError(t, reader.Close())
	}

	obj, err := stack.NewObject(ctx, name)
	require.NoError(t, err)
	require.Equal(t, logicalSize, obj.Size())

	boundary := 20 * largeTestChunkSize
	readRange := func(start, end int64) {
		t.Helper()
		r, err := obj.Open(ctx, &fs.RangeOption{Start: start, End: end - 1})
		require.NoError(t, err)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		require.NoError(t, r.Close())
		require.Len(t, got, int(end-start))
		require.Equal(t, make([]byte, len(got)), got)
	}
	readRange(boundary-64*1024, boundary+64*1024)
	readRange(boundary+64*1024, boundary+320*1024)

	reader, err := obj.Open(ctx)
	require.NoError(t, err)
	readbackHash := sha256.New()
	readbackSize, err := io.Copy(readbackHash, reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, logicalSize, readbackSize)
	require.Equal(t, hex.EncodeToString(sourceHash.Sum(nil)), hex.EncodeToString(readbackHash.Sum(nil)))
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
