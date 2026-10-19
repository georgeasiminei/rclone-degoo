package degoo

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/stretchr/testify/require"
)

func TestRmdirRemovesOrphanDirectoryMarkers(t *testing.T) {
	for _, category := range []int{0, 6} {
		t.Run(strconv.Itoa(category), func(t *testing.T) {
			mock := newMockDegoo()
			mock.items["folder"] = mockItem{id: "folder", name: "folder", parentID: "vault", category: 2}
			mock.items["marker"] = mockItem{id: "marker", name: directoryMarkerName("removed-placeholder"), parentID: "folder", category: category}
			f, server := newTestFs(t, mock)
			defer server.Close()
			ctx := context.Background()
			entries, err := f.List(ctx, "folder")
			require.NoError(t, err)
			require.Empty(t, entries)
			require.NoError(t, f.Rmdir(ctx, "folder"))
			mock.mu.Lock()
			defer mock.mu.Unlock()
			require.Len(t, mock.items, 1)
			require.Equal(t, []deleteCall{{id: "marker"}, {id: "folder"}}, mock.deleteCalls)
		})
	}
}

func TestRmdirCleansAbandonedStagedUploads(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			mock := newMockDegoo()
			mock.items["folder"] = mockItem{id: "folder", name: "folder", parentID: "vault", category: 2}
			stagedName, err := newInternalName(temporaryPrefix)
			require.NoError(t, err)
			mock.items["staged"] = mockItem{id: "staged", name: stagedName + ".bin", parentID: "folder", category: 0, size: 143}
			f, server, err := newTestFsAtRootID(t, mock, "", "vault", configmap.Simple{"rmdir_cleanup": strconv.FormatBool(enabled)})
			require.NoError(t, err)
			defer server.Close()
			err = f.Rmdir(context.Background(), "folder")
			if enabled {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, fs.ErrorDirectoryNotEmpty)
			}
			mock.mu.Lock()
			defer mock.mu.Unlock()
			if enabled {
				require.Len(t, mock.items, 1)
				require.Equal(t, []deleteCall{{id: "staged"}, {id: "folder"}}, mock.deleteCalls)
			} else {
				require.Empty(t, mock.deleteCalls)
			}
		})
	}
}

func TestRmdirPreservesUnrecognizedChildren(t *testing.T) {
	for _, tc := range []struct {
		name  string
		child mockItem
	}{
		{"visible_file", mockItem{name: "file", category: 6, size: 1}},
		{"visible_directory", mockItem{name: "nested", category: 2}},
		{"malformed_marker", mockItem{name: directoryMarkerPrefix + "invalid", category: 0}},
		{"nonempty_marker", mockItem{name: directoryMarkerName("removed-placeholder"), category: 6, size: 1}},
		{"directory_marker", mockItem{name: directoryMarkerName("removed-placeholder"), category: 2}},
		{"malformed_upload", mockItem{name: temporaryPrefix + "invalid.bin", category: 0}},
		{"upload_directory", mockItem{name: temporaryPrefix + "0123456789abcdef0123456789abcdef.bin", category: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := newMockDegoo()
			mock.items["folder"] = mockItem{id: "folder", name: "folder", parentID: "vault", category: 2}
			child := tc.child
			child.id = "child"
			child.parentID = "folder"
			mock.items[child.id] = child
			mock.items["staged"] = mockItem{id: "staged", name: temporaryPrefix + "0123456789abcdef0123456789abcdef.bin", parentID: "folder", category: 0, size: 143}
			f, server, err := newTestFsAtRootID(t, mock, "", "vault", configmap.Simple{"rmdir_cleanup": "true"})
			require.NoError(t, err)
			defer server.Close()
			require.ErrorIs(t, f.Rmdir(context.Background(), "folder"), fs.ErrorDirectoryNotEmpty)
			mock.mu.Lock()
			defer mock.mu.Unlock()
			require.Empty(t, mock.deleteCalls)
			require.Len(t, mock.items, 4)
		})
	}
}

func TestRmdirPreservesDirectoryWhenCleanupFails(t *testing.T) {
	mock := newMockDegoo()
	mock.items["folder"] = mockItem{id: "folder", name: "folder", parentID: "vault", category: 2}
	mock.items["staged"] = mockItem{id: "staged", name: temporaryPrefix + "0123456789abcdef0123456789abcdef.bin", parentID: "folder", category: 0, size: 143}
	mock.failDeleteIDs = map[string]bool{"staged": true}
	f, server, err := newTestFsAtRootID(t, mock, "", "vault", configmap.Simple{"rmdir_cleanup": "true"})
	require.NoError(t, err)
	defer server.Close()
	require.Error(t, f.Rmdir(context.Background(), "folder"))
	mock.mu.Lock()
	require.Contains(t, mock.items, "folder")
	require.Contains(t, mock.items, "staged")
	require.Equal(t, []deleteCall{{id: "staged"}}, mock.deleteCalls)
	mock.failDeleteIDs = nil
	mock.mu.Unlock()
	require.NoError(t, f.Rmdir(context.Background(), "folder"), "failed cleanup must release the directory barrier for retry")
}

func TestRmdirPreservesDataBelowAnInternalPlaceholder(t *testing.T) {
	mock := newMockDegoo()
	mock.items["folder"] = mockItem{id: "folder", name: "folder", parentID: "vault", category: 2}
	mock.items["marker"] = mockItem{id: "marker", name: directoryMarkerName("removed-placeholder"), parentID: "folder", category: 0}
	mock.items["nested-data"] = mockItem{id: "nested-data", name: "file", parentID: "marker", category: 6, size: 1}
	f, server := newTestFs(t, mock)
	defer server.Close()
	require.ErrorIs(t, f.Rmdir(context.Background(), "folder"), fs.ErrorDirectoryNotEmpty)
	mock.mu.Lock()
	defer mock.mu.Unlock()
	require.Empty(t, mock.deleteCalls)
	require.Len(t, mock.items, 4)
}

func TestRmdirDoesNotCleanAnActiveUpload(t *testing.T) {
	mock := newMockDegoo()
	mock.items["folder"] = mockItem{id: "folder", name: "folder", parentID: "vault", category: 2}
	f, server := newTestFs(t, mock)
	defer server.Close()
	f.opt.RmdirCleanup = true
	mock.blockUpload = true
	mock.uploadStarted = make(chan struct{}, 1)
	mock.uploadRelease = make(chan struct{})
	defer close(mock.uploadRelease)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcomes := startPuts(f, map[string][]byte{"folder/pending": []byte("generated in-flight content")}, map[string]context.Context{"folder/pending": ctx})
	select {
	case <-mock.uploadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	require.ErrorIs(t, f.Rmdir(context.Background(), "folder"), fs.ErrorDirectoryNotEmpty)
	mock.mu.Lock()
	require.Empty(t, mock.deleteCalls)
	require.Contains(t, mock.items, "folder")
	mock.mu.Unlock()
	cancel()
	result := collectPuts(t, outcomes, 1)
	require.Error(t, result["folder/pending"].err)
}

func TestDirectoryRemovalBlocksOnlyOverlappingMutations(t *testing.T) {
	manager := newPathLockManager()
	release, err := manager.beginDirectoryRemoval("folder")
	require.NoError(t, err)
	for _, path := range []string{"folder", "folder/file", "folder/nested/file", ""} {
		_, err := manager.acquire(context.Background(), path)
		require.Error(t, err)
	}
	unrelated, err := manager.acquire(context.Background(), "other/file")
	require.NoError(t, err)
	unrelated()
	release()
	writer, err := manager.acquire(context.Background(), "folder/file")
	require.NoError(t, err)
	_, err = manager.beginDirectoryRemoval("folder")
	require.ErrorIs(t, err, fs.ErrorDirectoryNotEmpty)
	writer()
	release, err = manager.beginDirectoryRemoval("folder")
	require.NoError(t, err)
	release()
}
