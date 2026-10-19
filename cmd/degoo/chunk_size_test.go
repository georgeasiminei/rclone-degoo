//go:build linux

package degoo

import (
	"bytes"
	"context"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func TestVaultReadsExistingFilesAfterIncreasingChunkSize(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	base, original := localStack(t, cfg)
	data := bytes.Repeat([]byte("generated chunk transition data "), 200)
	put(t, original, "old-chunks.bin", data)
	cfg.ChunkSize *= 8
	cfg.MaxObjectSize *= 4
	reopened, err := newStack(ctx, base, cfg)
	require.NoError(t, err)
	old, err := reopened.NewObject(ctx, "old-chunks.bin")
	require.NoError(t, err)
	require.Equal(t, data, read(t, old))
	require.Equal(t, data[950:2150], read(t, old, &fs.RangeOption{Start: 950, End: 2149}))
	newData := bytes.Repeat([]byte("generated larger chunk data "), 400)
	put(t, reopened, "new-chunks.bin", newData)
	fresh, err := newStack(ctx, base, cfg)
	require.NoError(t, err)
	for name, want := range map[string][]byte{"old-chunks.bin": data, "new-chunks.bin": newData} {
		obj, err := fresh.NewObject(ctx, name)
		require.NoError(t, err)
		require.Equal(t, want, read(t, obj))
	}
}
