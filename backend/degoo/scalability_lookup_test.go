package degoo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/chunker"
	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
)

// lookupTrace captures HTTP calls made by the mock and the backend's local
// metadata counters. It deliberately contains counts only, never IDs or names.
type lookupTrace struct {
	listCalls     int
	getItemCalls  int
	downloadCalls int
	stats         api.RequestStats
}

func snapshotLookupTrace(f *Fs, mock *mockDegoo) lookupTrace {
	mock.mu.Lock()
	listCalls := mock.listCalls
	getItemCalls := mock.getItemCalls
	downloadCalls := mock.downloadCalls
	mock.mu.Unlock()
	return lookupTrace{
		listCalls:     listCalls,
		getItemCalls:  getItemCalls,
		downloadCalls: downloadCalls,
		stats:         f.RequestStats(),
	}
}

func lookupTraceDelta(before, after lookupTrace) lookupTrace {
	return lookupTrace{
		listCalls:     after.listCalls - before.listCalls,
		getItemCalls:  after.getItemCalls - before.getItemCalls,
		downloadCalls: after.downloadCalls - before.downloadCalls,
		stats: api.RequestStats{
			MetadataCacheHits:             after.stats.MetadataCacheHits - before.stats.MetadataCacheHits,
			MetadataCacheMisses:           after.stats.MetadataCacheMisses - before.stats.MetadataCacheMisses,
			MetadataPages:                 after.stats.MetadataPages - before.stats.MetadataPages,
			MetadataMutationInvalidations: after.stats.MetadataMutationInvalidations - before.stats.MetadataMutationInvalidations,
			MetadataParentInvalidations:   after.stats.MetadataParentInvalidations - before.stats.MetadataParentInvalidations,
			MetadataGlobalInvalidations:   after.stats.MetadataGlobalInvalidations - before.stats.MetadataGlobalInvalidations,
			RequestsByOperation:           requestOperationDelta(before.stats.RequestsByOperation, after.stats.RequestsByOperation),
		},
	}
}

func requestOperationDelta(before, after map[string]uint64) map[string]uint64 {
	delta := make(map[string]uint64)
	for operation, count := range after {
		if count > before[operation] {
			delta[operation] = count - before[operation]
		}
	}
	return delta
}

func assertLookupTrace(t *testing.T, got lookupTrace, want lookupTrace) {
	t.Helper()
	if got.listCalls != want.listCalls || got.stats.MetadataPages != uint64(want.listCalls) {
		t.Errorf("listing requests/pages = %d/%d, want %d/%d", got.listCalls, got.stats.MetadataPages, want.listCalls, want.listCalls)
	}
	if got.getItemCalls != want.getItemCalls {
		t.Errorf("GetItem calls = %d, want %d", got.getItemCalls, want.getItemCalls)
	}
	if got.downloadCalls != want.downloadCalls {
		t.Errorf("download calls = %d, want %d", got.downloadCalls, want.downloadCalls)
	}
	if got.stats.MetadataCacheHits != want.stats.MetadataCacheHits || got.stats.MetadataCacheMisses != want.stats.MetadataCacheMisses {
		t.Errorf("metadata cache hit/miss delta = %d/%d, want %d/%d", got.stats.MetadataCacheHits, got.stats.MetadataCacheMisses, want.stats.MetadataCacheHits, want.stats.MetadataCacheMisses)
	}
	if got.stats.MetadataMutationInvalidations != want.stats.MetadataMutationInvalidations || got.stats.MetadataParentInvalidations != want.stats.MetadataParentInvalidations || got.stats.MetadataGlobalInvalidations != want.stats.MetadataGlobalInvalidations {
		t.Errorf("mutation/parent/global invalidations = %d/%d/%d, want %d/%d/%d", got.stats.MetadataMutationInvalidations, got.stats.MetadataParentInvalidations, got.stats.MetadataGlobalInvalidations, want.stats.MetadataMutationInvalidations, want.stats.MetadataParentInvalidations, want.stats.MetadataGlobalInvalidations)
	}
	if !reflect.DeepEqual(got.stats.RequestsByOperation, want.stats.RequestsByOperation) {
		t.Errorf("operation counts = %v, want exactly %v", got.stats.RequestsByOperation, want.stats.RequestsByOperation)
	}
}

func logLookupTrace(t *testing.T, label string, trace lookupTrace) {
	t.Helper()
	t.Logf("%s: list requests/pages=%d/%d, GetItem=%d, downloads=%d, cache hits/misses=%d/%d, mutation/parent/global invalidations=%d/%d/%d, operations=%v",
		label, trace.listCalls, trace.stats.MetadataPages, trace.getItemCalls, trace.downloadCalls,
		trace.stats.MetadataCacheHits, trace.stats.MetadataCacheMisses,
		trace.stats.MetadataMutationInvalidations, trace.stats.MetadataParentInvalidations,
		trace.stats.MetadataGlobalInvalidations, trace.stats.RequestsByOperation)
}

func TestScalabilityLookupTraceNewObjectAndOpen(t *testing.T) {
	ctx := context.Background()
	mock := newMockDegoo()
	content := []byte("generated encrypted payload")
	mock.items["opaque-object-id"] = mockItem{
		id: "opaque-object-id", name: "cipher", parentID: "vault", category: 6,
		size: int64(len(content)), content: content,
	}
	f, server := newTestFs(t, mock)
	defer server.Close()

	before := snapshotLookupTrace(f, mock)
	obj, err := f.NewObject(ctx, "cipher")
	if err != nil {
		t.Fatalf("cold NewObject: %v", err)
	}
	cold := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
	logLookupTrace(t, "cold NewObject", cold)
	assertLookupTrace(t, cold, lookupTrace{
		listCalls: 1,
		stats: api.RequestStats{
			MetadataCacheMisses: 1,
			MetadataPages:       1,
			RequestsByOperation: map[string]uint64{"GetFileChildren5": 1},
		},
	})

	before = snapshotLookupTrace(f, mock)
	if _, err := f.NewObject(ctx, "cipher"); err != nil {
		t.Fatalf("warm NewObject: %v", err)
	}
	warm := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
	logLookupTrace(t, "warm NewObject", warm)
	assertLookupTrace(t, warm, lookupTrace{
		stats: api.RequestStats{MetadataCacheHits: 1, RequestsByOperation: map[string]uint64{}},
	})

	before = snapshotLookupTrace(f, mock)
	body, err := obj.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, readErr := io.ReadAll(body)
	closeTestBody(t, body)
	if readErr != nil || !bytes.Equal(got, content) {
		t.Fatalf("opened bytes = %q, read error = %v", got, readErr)
	}
	open := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
	logLookupTrace(t, "open with fresh listed URL", open)
	assertLookupTrace(t, open, lookupTrace{
		downloadCalls: 1,
		stats:         api.RequestStats{RequestsByOperation: map[string]uint64{"download": 1}},
	})
}

func TestScalabilityLookupTraceCreateOverwriteRenameDelete(t *testing.T) {
	ctx := context.Background()
	content := []byte("generated encrypted write")
	createdAt := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	t.Run("create", func(t *testing.T) {
		mock := newMockDegoo()
		f, server := newTestFs(t, mock)
		defer server.Close()

		before := snapshotLookupTrace(f, mock)
		if _, err := f.Put(ctx, bytes.NewReader(content), object.NewStaticObjectInfo("created", createdAt, int64(len(content)), true, nil, f)); err != nil {
			t.Fatalf("Put create: %v", err)
		}
		trace := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
		logLookupTrace(t, "create", trace)
		assertLookupTrace(t, trace, lookupTrace{
			listCalls: 3, getItemCalls: 2, downloadCalls: 1,
			stats: api.RequestStats{
				MetadataCacheHits:             1,
				MetadataCacheMisses:           1,
				MetadataPages:                 3,
				MetadataMutationInvalidations: 2,
				MetadataParentInvalidations:   8,
				RequestsByOperation: map[string]uint64{
					"GCS upload":          1,
					"GetBucketWriteAuth4": 1,
					"GetFileChildren5":    3,
					"GetOverlay4":         2,
					"SetRenameFile":       1,
					"SetUploadFile3":      1,
					"download":            1,
				},
			},
		})
	})

	t.Run("overwrite", func(t *testing.T) {
		mock := newMockDegoo()
		oldContent := []byte("old generated payload")
		mock.items["old-object-id"] = mockItem{
			id: "old-object-id", name: "replace", parentID: "vault", category: 6,
			size: int64(len(oldContent)), content: oldContent,
		}
		f, server := newTestFs(t, mock)
		defer server.Close()

		before := snapshotLookupTrace(f, mock)
		if _, err := f.Put(ctx, bytes.NewReader(content), object.NewStaticObjectInfo("replace", createdAt, int64(len(content)), true, nil, f)); err != nil {
			t.Fatalf("Put overwrite: %v", err)
		}
		trace := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
		logLookupTrace(t, "overwrite", trace)
		assertLookupTrace(t, trace, lookupTrace{
			listCalls: 4, getItemCalls: 2, downloadCalls: 1,
			stats: api.RequestStats{
				MetadataCacheHits:             1,
				MetadataCacheMisses:           2,
				MetadataPages:                 4,
				MetadataMutationInvalidations: 4,
				MetadataParentInvalidations:   12,
				RequestsByOperation: map[string]uint64{
					"GCS upload":          1,
					"GetBucketWriteAuth4": 1,
					"GetFileChildren5":    4,
					"GetOverlay4":         2,
					"SetDeleteFile5":      1,
					"SetRenameFile":       2,
					"SetUploadFile3":      1,
					"download":            1,
				},
			},
		})
	})

	t.Run("rename", func(t *testing.T) {
		mock := newMockDegoo()
		mock.items["rename-object-id"] = mockItem{
			id: "rename-object-id", name: "before", parentID: "vault", category: 6, size: 3,
			content: []byte("old"),
		}
		f, server := newTestFs(t, mock)
		defer server.Close()
		obj, err := f.NewObject(ctx, "before")
		if err != nil {
			t.Fatalf("NewObject before rename: %v", err)
		}

		before := snapshotLookupTrace(f, mock)
		if _, err := f.Move(ctx, obj, "after"); err != nil {
			t.Fatalf("Move rename: %v", err)
		}
		trace := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
		logLookupTrace(t, "rename", trace)
		assertLookupTrace(t, trace, lookupTrace{
			listCalls: 1,
			stats: api.RequestStats{
				MetadataCacheHits:             1,
				MetadataCacheMisses:           1,
				MetadataPages:                 1,
				MetadataMutationInvalidations: 1,
				MetadataParentInvalidations:   2,
				RequestsByOperation: map[string]uint64{
					"GetFileChildren5": 1,
					"SetRenameFile":    1,
				},
			},
		})
	})

	t.Run("delete", func(t *testing.T) {
		mock := newMockDegoo()
		mock.items["delete-object-id"] = mockItem{
			id: "delete-object-id", name: "remove", parentID: "vault", category: 6, size: 3,
			content: []byte("old"),
		}
		f, server := newTestFs(t, mock)
		defer server.Close()
		obj, err := f.NewObject(ctx, "remove")
		if err != nil {
			t.Fatalf("NewObject before delete: %v", err)
		}

		before := snapshotLookupTrace(f, mock)
		if err := obj.Remove(ctx); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		trace := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
		logLookupTrace(t, "delete", trace)
		assertLookupTrace(t, trace, lookupTrace{
			listCalls: 1,
			stats: api.RequestStats{
				MetadataCacheHits:             1,
				MetadataPages:                 1,
				MetadataMutationInvalidations: 1,
				MetadataParentInvalidations:   4,
				RequestsByOperation: map[string]uint64{
					"GetFileChildren5": 1,
					"SetDeleteFile5":   1,
				},
			},
		})
	})
}

func TestScalabilityLookupDirectGetItemUsesOpaqueID(t *testing.T) {
	for _, siblingCount := range []int{0, 256} {
		t.Run(fmt.Sprintf("%d_siblings", siblingCount), func(t *testing.T) {
			mock := newMockDegoo()
			mock.items["opaque-target-id"] = mockItem{
				id: "opaque-target-id", name: "encrypted-name", parentID: "vault", category: 6, size: 1,
				content: []byte("x"),
			}
			for i := 0; i < siblingCount; i++ {
				id := fmt.Sprintf("sibling-id-%04d", i)
				mock.items[id] = mockItem{id: id, name: fmt.Sprintf("encrypted-sibling-%04d", i), parentID: "vault", category: 6, size: 1}
			}
			f, server := newTestFs(t, mock)
			defer server.Close()

			before := snapshotLookupTrace(f, mock)
			item, err := f.getItem(context.Background(), "opaque-target-id")
			if err != nil {
				t.Fatalf("GetItem by opaque ID: %v", err)
			}
			if item.ID != "opaque-target-id" {
				t.Fatalf("GetItem ID = %q, want opaque target ID", item.ID)
			}
			trace := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
			logLookupTrace(t, "direct GetItem", trace)
			assertLookupTrace(t, trace, lookupTrace{
				getItemCalls: 1,
				stats:        api.RequestStats{RequestsByOperation: map[string]uint64{"GetOverlay4": 1}},
			})

			before = snapshotLookupTrace(f, mock)
			_, err = f.getItem(context.Background(), "missing-opaque-id")
			if err == nil {
				t.Fatal("GetItem for a missing ID succeeded")
			}
			if errors.Is(err, fs.ErrorObjectNotFound) {
				t.Fatalf("direct GetItem miss was mapped to filesystem not-found: %v", err)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("direct GetItem miss error = %T, want provider API error", err)
			}
			miss := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
			logLookupTrace(t, "direct GetItem miss", miss)
			assertLookupTrace(t, miss, lookupTrace{
				getItemCalls: 1,
				stats:        api.RequestStats{RequestsByOperation: map[string]uint64{"GetOverlay4": 1}},
			})
		})
	}
}

func TestScalabilityLookupTraceChunkerManifestAndSiblings(t *testing.T) {
	for _, siblingCount := range []int{0, 256} {
		t.Run(fmt.Sprintf("%d_siblings", siblingCount), func(t *testing.T) {
			mock := newMockDegoo()
			manifest := []byte(`{"ver":1,"size":8,"nchunks":2}`)
			mock.items["manifest-id"] = mockItem{
				id: "manifest-id", name: "logical.bin", parentID: "vault", category: 6,
				size: int64(len(manifest)), content: manifest,
			}
			mock.items["chunk-one-id"] = mockItem{
				id: "chunk-one-id", name: "logical.bin.rclone_chunk.001", parentID: "vault", category: 6,
				size: 4, content: []byte("abcd"),
			}
			mock.items["chunk-two-id"] = mockItem{
				id: "chunk-two-id", name: "logical.bin.rclone_chunk.002", parentID: "vault", category: 6,
				size: 4, content: []byte("efgh"),
			}
			for i := 0; i < siblingCount; i++ {
				id := fmt.Sprintf("unrelated-id-%04d", i)
				mock.items[id] = mockItem{
					id: id, name: fmt.Sprintf("unrelated-%04d", i), parentID: "vault", category: 6, size: 1,
				}
			}
			f, server := newTestFs(t, mock)
			defer server.Close()

			// Use the real chunker wrapper over a fresh Degoo client pointed at
			// the same generated mock vault. This exercises chunker discovery
			// without coupling its cache to the test Fs above.
			baseConfig := configmap.Simple{
				"api_key":        f.opt.APIKey,
				"api_url":        f.opt.APIURL,
				"auth_file":      f.opt.AuthFile,
				"rest_url":       f.opt.RESTURL,
				"root_folder_id": f.opt.RootFolderID,
			}.String()
			wrappedFs, err := chunker.NewFs(context.Background(), "lookup-trace-chunker", "", configmap.Simple{
				"remote":       ":degoo," + baseConfig + ":",
				"chunk_size":   "4",
				"name_format":  "*.rclone_chunk.###",
				"start_from":   "1",
				"meta_format":  "simplejson",
				"hash_type":    "md5",
				"transactions": "rename",
			})
			if err != nil {
				t.Fatalf("chunker.NewFs: %v", err)
			}
			chunkerFs, ok := wrappedFs.(*chunker.Fs)
			if !ok {
				t.Fatalf("chunker filesystem type = %T, want *chunker.Fs", wrappedFs)
			}
			baseFs, ok := chunkerFs.UnWrap().(*Fs)
			if !ok {
				t.Fatalf("chunker base filesystem type = %T, want *degoo.Fs", chunkerFs.UnWrap())
			}

			before := snapshotLookupTrace(baseFs, mock)
			obj, err := wrappedFs.NewObject(context.Background(), "logical.bin")
			if err != nil {
				t.Fatalf("chunker NewObject: %v", err)
			}
			if got := obj.Size(); got != 8 {
				t.Fatalf("chunker logical size = %d, want 8", got)
			}
			newObject := lookupTraceDelta(before, snapshotLookupTrace(baseFs, mock))
			logLookupTrace(t, fmt.Sprintf("chunker NewObject with %d siblings", siblingCount), newObject)
			assertLookupTrace(t, newObject, lookupTrace{
				listCalls: 1,
				stats: api.RequestStats{
					MetadataCacheHits:   1,
					MetadataCacheMisses: 1,
					MetadataPages:       1,
					RequestsByOperation: map[string]uint64{"GetFileChildren5": 1},
				},
			})

			// The manifest body is read lazily by chunker.Object.Open before it
			// opens the data chunks. That path uses byte ranges in this wrapper;
			// the shared mock's download handler supports only full-object GETs.
		})
	}
}
