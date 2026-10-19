package degoo

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs/object"
	"github.com/stretchr/testify/require"
)

func TestEncryptedRmdirCleansInterruptedEmptyFileUpload(t *testing.T) {
	ctx := context.Background()
	mock := newMockDegoo()
	options := installedEncryptedChunkerOptions
	options.RmdirCleanup = true
	stack, _ := newEncryptedChunkerStack(t, mock, options)
	require.NoError(t, stack.Mkdir(ctx, "folder"))
	cipher := stack.UnWrap().(*crypt.Fs)
	orphanName := "folder/pending.rclone_chunk.001_abcd"
	orphan, err := cipher.Put(ctx, bytes.NewReader(nil), object.NewStaticObjectInfo(orphanName, time.Now(), 0, true, nil, cipher))
	require.NoError(t, err)
	raw := orphan.(*crypt.Object).UnWrap().(*Object)
	mock.mu.Lock()
	chunk := mock.items[raw.ID()]
	chunk.category = 0
	mock.items[chunk.id] = chunk
	mock.items["staged-manifest"] = mockItem{
		id: "staged-manifest", name: temporaryPrefix + "0123456789abcdef0123456789abcdef.bin",
		parentID: raw.ParentID(), category: 0, size: 143,
	}
	mock.mu.Unlock()
	cipher.UnWrap().(*Fs).api.ClearMetadataCache()
	entries, err := stack.List(ctx, "folder")
	require.NoError(t, err)
	require.Empty(t, entries, "an interrupted upload must remain hidden from the logical view")
	require.NoError(t, stack.Rmdir(ctx, "folder"))
	mock.mu.Lock()
	defer mock.mu.Unlock()
	require.Len(t, mock.items, 1, "only the vault root may remain after deleting the directory")
	for _, call := range mock.deleteCalls {
		require.False(t, call.permanent, "remnants and directories must go to the recycle bin")
	}
}
