package degoo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/chunker"
	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/object"
)

const scalabilitySiblingCount = 1005

func addScalabilitySiblings(mock *mockDegoo, count int, includeTarget bool) {
	mock.listPageSize = 1000
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("generated-sibling-%04d", i)
		mock.items[id] = mockItem{
			id: id, name: fmt.Sprintf("generated-sibling-%04d", i), parentID: "vault",
			category: 6, size: 1, content: []byte("x"),
		}
	}
	if includeTarget {
		mock.items["z-generated-target-id"] = mockItem{
			id: "z-generated-target-id", name: "generated-target", parentID: "vault",
			category: 6, size: 1, content: []byte("x"),
		}
	}
}

func assertPaginatedLookup(t *testing.T, got lookupTrace, pages int) {
	t.Helper()
	if got.listCalls != pages || got.stats.MetadataPages != uint64(pages) {
		t.Fatalf("listing requests/pages = %d/%d, want %d/%d", got.listCalls, got.stats.MetadataPages, pages, pages)
	}
	if got.stats.MetadataCacheMisses == 0 {
		t.Fatalf("paginated lookup had no cache miss: %+v", got.stats)
	}
	if got.stats.RequestsByOperation["GetFileChildren5"] != uint64(pages) {
		t.Fatalf("GetFileChildren5 requests = %d, want %d", got.stats.RequestsByOperation["GetFileChildren5"], pages)
	}
}

func assertPaginatedMutation(t *testing.T, trace lookupTrace, pages int, mutation string, invalidations, parentInvalidations uint64) {
	t.Helper()
	if trace.listCalls != pages || trace.stats.MetadataPages != uint64(pages) {
		t.Fatalf("mutation listing requests/pages = %d/%d, want %d/%d", trace.listCalls, trace.stats.MetadataPages, pages, pages)
	}
	if trace.stats.MetadataMutationInvalidations != invalidations || trace.stats.MetadataParentInvalidations != parentInvalidations || trace.stats.MetadataGlobalInvalidations != 0 {
		t.Fatalf("mutation/parent/global invalidations = %d/%d/%d, want %d/%d/0", trace.stats.MetadataMutationInvalidations, trace.stats.MetadataParentInvalidations, trace.stats.MetadataGlobalInvalidations, invalidations, parentInvalidations)
	}
	if got := trace.stats.RequestsByOperation[mutation]; got != 1 {
		t.Fatalf("mutation operation %q requests = %d, want 1: %v", mutation, got, trace.stats.RequestsByOperation)
	}
	logLookupTrace(t, "paginated "+mutation, trace)
}

func TestScalabilityPaginatedMutations(t *testing.T) {
	ctx := context.Background()
	createdAt := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	content := []byte("generated paginated mutation payload")

	t.Run("lookup_refresh_and_create", func(t *testing.T) {
		mock := newMockDegoo()
		addScalabilitySiblings(mock, scalabilitySiblingCount, true)
		f, server := newTestFs(t, mock)
		defer server.Close()

		before := snapshotLookupTrace(f, mock)
		obj, err := f.NewObject(ctx, "generated-target")
		if err != nil {
			t.Fatalf("cold NewObject: %v", err)
		}
		cold := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
		assertPaginatedLookup(t, cold, 2)
		if got := obj.Size(); got != 1 {
			t.Fatalf("target size = %d, want 1", got)
		}

		before = snapshotLookupTrace(f, mock)
		if _, err := f.NewObject(ctx, "generated-target"); err != nil {
			t.Fatalf("warm NewObject: %v", err)
		}
		warm := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
		if warm.listCalls != 0 || warm.stats.MetadataPages != 0 || warm.getItemCalls != 0 {
			t.Fatalf("warm lookup did remote work: %+v", warm)
		}
		if warm.stats.MetadataCacheHits == 0 {
			t.Fatalf("warm lookup did not use the listing snapshot: %+v", warm.stats)
		}

		f.api.ClearMetadataCache()
		before = snapshotLookupTrace(f, mock)
		if _, err := f.NewObject(ctx, "generated-target"); err != nil {
			t.Fatalf("NewObject after refresh: %v", err)
		}
		refreshed := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
		assertPaginatedLookup(t, refreshed, 2)

		f.api.ClearMetadataCache()
		before = snapshotLookupTrace(f, mock)
		if _, err := f.Put(ctx, bytes.NewReader(content), object.NewStaticObjectInfo("created-file", createdAt, int64(len(content)), true, nil, f)); err != nil {
			t.Fatalf("Put create in paginated directory: %v", err)
		}
		create := lookupTraceDelta(before, snapshotLookupTrace(f, mock))
		assertPaginatedMutation(t, create, 6, "SetUploadFile3", 2, 8)
		created, err := f.NewObject(ctx, "created-file")
		if err != nil {
			t.Fatalf("NewObject after paginated create: %v", err)
		}
		if got := readObject(t, created); !bytes.Equal(got, content) {
			t.Fatalf("paginated create readback = %q, want %q", got, content)
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		mock := newMockDegoo()
		addScalabilitySiblings(mock, scalabilitySiblingCount, false)
		mock.items["z-overwrite-target-id"] = mockItem{
			id: "z-overwrite-target-id", name: "overwrite-target", parentID: "vault",
			category: 6, size: 3, content: []byte("old"),
		}
		f, server := newTestFs(t, mock)
		defer server.Close()

		traceBefore := snapshotLookupTrace(f, mock)
		if _, err := f.Put(ctx, bytes.NewReader(content), object.NewStaticObjectInfo("overwrite-target", createdAt, int64(len(content)), true, nil, f)); err != nil {
			t.Fatalf("Put overwrite in paginated directory: %v", err)
		}
		trace := lookupTraceDelta(traceBefore, snapshotLookupTrace(f, mock))
		assertPaginatedMutation(t, trace, 8, "SetUploadFile3", 4, 12)
		updated, err := f.NewObject(ctx, "overwrite-target")
		if err != nil {
			t.Fatalf("NewObject after paginated overwrite: %v", err)
		}
		if got := readObject(t, updated); !bytes.Equal(got, content) {
			t.Fatalf("paginated overwrite readback = %q, want %q", got, content)
		}
	})

	t.Run("rename", func(t *testing.T) {
		mock := newMockDegoo()
		addScalabilitySiblings(mock, scalabilitySiblingCount, false)
		mock.items["z-rename-target-id"] = mockItem{
			id: "z-rename-target-id", name: "rename-target", parentID: "vault",
			category: 6, size: 3, content: []byte("old"),
		}
		f, server := newTestFs(t, mock)
		defer server.Close()
		obj, err := f.NewObject(ctx, "rename-target")
		if err != nil {
			t.Fatalf("NewObject before rename: %v", err)
		}

		traceBefore := snapshotLookupTrace(f, mock)
		if _, err := f.Move(ctx, obj, "renamed-target"); err != nil {
			t.Fatalf("Move in paginated directory: %v", err)
		}
		assertPaginatedMutation(t, lookupTraceDelta(traceBefore, snapshotLookupTrace(f, mock)), 2, "SetRenameFile", 1, 2)
		if _, err := f.NewObject(ctx, "renamed-target"); err != nil {
			t.Fatalf("NewObject at renamed path: %v", err)
		}
		if _, err := f.NewObject(ctx, "rename-target"); !errors.Is(err, fs.ErrorObjectNotFound) {
			t.Fatalf("NewObject at old path error = %v, want not found", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		mock := newMockDegoo()
		addScalabilitySiblings(mock, scalabilitySiblingCount, false)
		mock.items["z-delete-target-id"] = mockItem{
			id: "z-delete-target-id", name: "delete-target", parentID: "vault",
			category: 6, size: 3, content: []byte("old"),
		}
		f, server := newTestFs(t, mock)
		defer server.Close()
		obj, err := f.NewObject(ctx, "delete-target")
		if err != nil {
			t.Fatalf("NewObject before delete: %v", err)
		}

		traceBefore := snapshotLookupTrace(f, mock)
		if err := obj.Remove(ctx); err != nil {
			t.Fatalf("Remove in paginated directory: %v", err)
		}
		assertPaginatedMutation(t, lookupTraceDelta(traceBefore, snapshotLookupTrace(f, mock)), 2, "SetDeleteFile5", 1, 4)
		if _, err := f.NewObject(ctx, "delete-target"); !errors.Is(err, fs.ErrorObjectNotFound) {
			t.Fatalf("NewObject after delete error = %v, want not found", err)
		}
	})
}

type encryptedChunkerStackOptions struct {
	HashType     string
	Transactions string
	FailHard     bool
	WrapHandler  func(http.Handler) http.Handler
}

var tracedEncryptedChunkerOptions = encryptedChunkerStackOptions{
	HashType:     "md5",
	Transactions: "rename",
}

var installedEncryptedChunkerOptions = encryptedChunkerStackOptions{
	HashType:     "sha1all",
	Transactions: "norename",
	FailHard:     true,
}

func newEncryptedChunkerStack(t *testing.T, mock *mockDegoo, options encryptedChunkerStackOptions) (*chunker.Fs, *Fs) {
	t.Helper()
	var base *Fs
	var server *httptest.Server
	if options.WrapHandler == nil {
		base, server = newTestFs(t, mock)
	} else {
		f, testServer, err := newTestFsAtRootIDWithHandler(t, options.WrapHandler(mock), "", "vault")
		if err != nil {
			testServer.Close()
			t.Fatalf("NewFs with wrapped handler: %v", err)
		}
		base = f.(*Fs)
		server = testServer
	}
	t.Cleanup(server.Close)

	degooRemote := ":degoo," + (configmap.Simple{
		"api_key":        base.opt.APIKey,
		"api_url":        base.opt.APIURL,
		"auth_file":      base.opt.AuthFile,
		"rest_url":       base.opt.RESTURL,
		"root_folder_id": base.opt.RootFolderID,
	}).String() + ":"
	cryptRemote := ":crypt," + (configmap.Simple{
		"remote":              degooRemote,
		"password":            obscure.MustObscure("generated-vault-password"),
		"password2":           obscure.MustObscure("generated-vault-salt"),
		"filename_encryption": "standard",
	}).String() + ":"
	wrapped, err := chunker.NewFs(context.Background(), "scalability-chunker", "", configmap.Simple{
		"remote":       cryptRemote,
		"chunk_size":   "128B",
		"name_format":  "*.rclone_chunk.###",
		"start_from":   "1",
		"meta_format":  "simplejson",
		"hash_type":    options.HashType,
		"transactions": options.Transactions,
		"fail_hard":    fmt.Sprint(options.FailHard),
	})
	if err != nil {
		t.Fatalf("chunker.NewFs over crypt: %v", err)
	}
	chunkerFs, ok := wrapped.(*chunker.Fs)
	if !ok {
		t.Fatalf("wrapped filesystem = %T, want *chunker.Fs", wrapped)
	}
	cryptFs, ok := chunkerFs.UnWrap().(*crypt.Fs)
	if !ok {
		t.Fatalf("chunker base = %T, want *crypt.Fs", chunkerFs.UnWrap())
	}
	degooFs, ok := cryptFs.UnWrap().(*Fs)
	if !ok {
		t.Fatalf("crypt base = %T, want *degoo.Fs", cryptFs.UnWrap())
	}
	return chunkerFs, degooFs
}

func mockRangeDownloadCount(mock *mockDegoo) int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return mock.rangeDownloadCalls
}

func mockVaultDataObjectCount(mock *mockDegoo) int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	count := 0
	for _, item := range mock.items {
		if item.parentID == "vault" && item.category == 6 && item.size > 0 {
			count++
		}
	}
	return count
}

func assertEncryptedRemoteState(t *testing.T, mock *mockDegoo, plaintexts ...[]byte) int {
	t.Helper()
	mock.mu.Lock()
	items := make([]mockItem, 0, len(mock.items))
	for _, item := range mock.items {
		if item.parentID == "vault" && item.category == 6 && item.size > 0 {
			item.content = append([]byte(nil), item.content...)
			items = append(items, item)
		}
	}
	mock.mu.Unlock()
	if len(items) < 3 {
		t.Fatalf("remote file object count = %d, want an encrypted manifest and multiple data chunks", len(items))
	}
	for _, item := range items {
		if strings.Contains(item.name, "private-document") || strings.Contains(item.name, "renamed-document") || strings.Contains(item.name, ".rclone_chunk.") {
			t.Fatalf("remote name leaked logical path or chunk layout: %q", item.name)
		}
		if len(item.content) == 0 {
			t.Fatalf("remote object %q has no generated payload", item.name)
		}
		if bytes.Contains(item.content, []byte(`"nchunks"`)) || bytes.Contains(item.content, []byte(`"size"`)) {
			t.Fatalf("remote object %q contains plaintext chunk metadata", item.name)
		}
		for _, plaintext := range plaintexts {
			if bytes.Contains(item.content, plaintext) {
				t.Fatalf("remote object %q contains plaintext user data", item.name)
			}
		}
	}
	return len(items)
}

func readObject(t *testing.T, obj fs.Object) []byte {
	t.Helper()
	body, err := obj.Open(context.Background())
	if err != nil {
		t.Fatalf("Open %q: %v", obj.Remote(), err)
	}
	data, readErr := io.ReadAll(body)
	closeTestBody(t, body)
	if readErr != nil {
		t.Fatalf("read %q: %v", obj.Remote(), readErr)
	}
	return data
}

func TestScalabilityEncryptedChunkerTrace(t *testing.T) {
	runEncryptedChunkerLifecycle(t, tracedEncryptedChunkerOptions)
}

func TestScalabilityEncryptedChunkerInstalledSettingsLifecycle(t *testing.T) {
	runEncryptedChunkerLifecycle(t, installedEncryptedChunkerOptions)
}

type sourceFailureReader struct{}

func (sourceFailureReader) Read([]byte) (int, error) {
	return 0, errors.New("injected source failure")
}

func TestScalabilityEncryptedChunkerInstalledSettingsFailedOverwritePreservesOldVersion(t *testing.T) {
	for _, failAfter := range []int{0, 127, 128, 129, 511, 623} {
		t.Run(fmt.Sprint(failAfter), func(t *testing.T) {
			ctx := context.Background()
			mock := newMockDegoo()
			stack, base := newEncryptedChunkerStack(t, mock, installedEncryptedChunkerOptions)
			original := bytes.Repeat([]byte("old complete version;"), 31)
			name := "generated-recovery-file"
			info := func(size int) fs.ObjectInfo {
				return object.NewStaticObjectInfo(name, time.Now(), int64(size), true, nil, stack)
			}
			if _, err := stack.Put(ctx, bytes.NewReader(original), info(len(original))); err != nil {
				t.Fatalf("initial Put: %v", err)
			}

			replacement := bytes.Repeat([]byte("replacement;"), 60)
			reader := io.MultiReader(bytes.NewReader(replacement[:failAfter]), sourceFailureReader{})
			if _, err := stack.Put(ctx, reader, info(len(replacement))); err == nil {
				t.Fatal("incomplete overwrite succeeded")
			}
			assertEncryptedReadback(t, stack, base, name, original, "failed source read")
		})
	}
}

func TestScalabilityEncryptedChunkerInstalledSettingsManifestPublishFailurePreservesOldVersion(t *testing.T) {
	ctx := context.Background()
	mock := newMockDegoo()
	var armed, triggered atomic.Bool
	var stagedObjects atomic.Int32
	var target string
	var originalObjectCount int
	options := installedEncryptedChunkerOptions
	options.WrapHandler = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if !armed.Load() || request.Method != http.MethodPost || request.URL.Path != "/" {
				next.ServeHTTP(writer, request)
				return
			}
			raw, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read GraphQL request: %v", err)
				http.Error(writer, "request read failed", http.StatusBadRequest)
				return
			}
			request.Body = io.NopCloser(bytes.NewReader(raw))
			var input struct {
				Operation string                     `json:"operationName"`
				Variables map[string]json.RawMessage `json:"variables"`
			}
			if json.Unmarshal(raw, &input) == nil && input.Operation == "SetRenameFile" {
				var rows []struct {
					Name string `json:"NewName"`
				}
				if json.Unmarshal(input.Variables["FileRenames"], &rows) == nil && len(rows) == 1 && rows[0].Name == target {
					if count := mockVaultDataObjectCount(mock); count > originalObjectCount && triggered.CompareAndSwap(false, true) {
						stagedObjects.Store(int32(count))
						writeTestJSON(writer, map[string]any{"errors": []any{map[string]string{"message": "injected manifest publication failure"}}})
						return
					}
				}
			}
			next.ServeHTTP(writer, request)
		})
	}
	stack, base := newEncryptedChunkerStack(t, mock, options)
	name := "generated-manifest-recovery"
	target = stack.UnWrap().(*crypt.Fs).EncryptFileName(name)
	info := func(size int) fs.ObjectInfo {
		return object.NewStaticObjectInfo(name, time.Now(), int64(size), true, nil, stack)
	}
	original := bytes.Repeat([]byte("old complete version;"), 31)
	if _, err := stack.Put(ctx, bytes.NewReader(original), info(len(original))); err != nil {
		t.Fatalf("initial Put: %v", err)
	}
	originalObjectCount = mockVaultDataObjectCount(mock)
	armed.Store(true)
	replacement := bytes.Repeat([]byte("replacement;"), 60)
	if _, err := stack.Put(ctx, bytes.NewReader(replacement), info(len(replacement))); err == nil {
		t.Fatal("manifest publication failure reported success")
	}
	if !triggered.Load() {
		t.Fatal("manifest publication rejection was not injected after staging")
	}
	if got := int(stagedObjects.Load()); got <= originalObjectCount {
		t.Fatalf("staged objects at manifest rejection = %d, want more than the original %d", got, originalObjectCount)
	}
	assertEncryptedReadback(t, stack, base, name, original, "failed manifest publication")
}

func assertEncryptedReadback(t *testing.T, stack *chunker.Fs, base *Fs, name string, want []byte, failure string) {
	t.Helper()
	base.DirCacheFlush()
	recovered, err := stack.NewObject(context.Background(), name)
	if err != nil {
		t.Fatalf("old version absent after %s: %v", failure, err)
	}
	if got := readObject(t, recovered); !bytes.Equal(got, want) {
		t.Fatalf("old version changed after %s: got %d bytes, want exact prior %d-byte version", failure, len(got), len(want))
	}
}

func runEncryptedChunkerLifecycle(t *testing.T, options encryptedChunkerStackOptions) {
	t.Helper()
	ctx := context.Background()
	mock := newMockDegoo()
	stack, base := newEncryptedChunkerStack(t, mock, options)
	name := "private-document.bin"
	createdAt := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	first := bytes.Repeat([]byte("generated-private-payload-"), 24)

	before := snapshotLookupTrace(base, mock)
	created, err := stack.Put(ctx, bytes.NewReader(first), object.NewStaticObjectInfo(name, createdAt, int64(len(first)), true, nil, stack))
	if err != nil {
		t.Fatalf("chunker Put through crypt: %v", err)
	}
	createTrace := lookupTraceDelta(before, snapshotLookupTrace(base, mock))
	if createTrace.stats.RequestsByOperation["SetUploadFile3"] == 0 || createTrace.stats.RequestsByOperation["GCS upload"] == 0 {
		t.Fatalf("encrypted create did not reach Degoo upload and registration: %v", createTrace.stats.RequestsByOperation)
	}
	logLookupTrace(t, "encrypted create", createTrace)
	remoteObjects := assertEncryptedRemoteState(t, mock, first, []byte("generated-private-payload-"))
	wantRemoteObjects := (len(first)+127)/128 + 1
	if remoteObjects != wantRemoteObjects {
		t.Fatalf("encrypted chunk object count = %d, want %d data chunks plus one manifest", remoteObjects, wantRemoteObjects)
	}
	t.Logf("encrypted chunker create: remote objects=%d, operations=%v", remoteObjects, createTrace.stats.RequestsByOperation)
	if got := created.Size(); got != int64(len(first)) {
		t.Fatalf("created logical size = %d, want %d", got, len(first))
	}

	base.DirCacheFlush()
	before = snapshotLookupTrace(base, mock)
	coldObject, err := stack.NewObject(ctx, name)
	if err != nil {
		t.Fatalf("cold encrypted NewObject: %v", err)
	}
	cold := lookupTraceDelta(before, snapshotLookupTrace(base, mock))
	logLookupTrace(t, "encrypted cold NewObject", cold)
	if cold.listCalls == 0 || cold.stats.MetadataPages != uint64(cold.listCalls) {
		t.Fatalf("cold encrypted lookup did not scan Degoo metadata: %+v", cold)
	}
	if got := coldObject.Size(); got != int64(len(first)) {
		t.Fatalf("cold logical size = %d, want %d", got, len(first))
	}

	before = snapshotLookupTrace(base, mock)
	warmObject, err := stack.NewObject(ctx, name)
	if err != nil {
		t.Fatalf("warm encrypted NewObject: %v", err)
	}
	warm := lookupTraceDelta(before, snapshotLookupTrace(base, mock))
	logLookupTrace(t, "encrypted warm NewObject", warm)
	wantWarmDownloads := 0
	if options.Transactions == "norename" {
		wantWarmDownloads = 1 // No-rename transactions read their manifest to select the active chunks.
	}
	if warm.listCalls != 0 || warm.getItemCalls != 0 || warm.downloadCalls != wantWarmDownloads {
		t.Fatalf("warm encrypted lookup made unexpected remote requests (downloads=%d, want %d): %+v", warm.downloadCalls, wantWarmDownloads, warm)
	}
	if got := warmObject.Size(); got != int64(len(first)) {
		t.Fatalf("warm logical size = %d, want %d", got, len(first))
	}

	before = snapshotLookupTrace(base, mock)
	got := readObject(t, coldObject)
	open := lookupTraceDelta(before, snapshotLookupTrace(base, mock))
	logLookupTrace(t, "encrypted cold open", open)
	if !bytes.Equal(got, first) {
		t.Fatalf("decrypted chunker read differs from uploaded bytes")
	}
	if open.downloadCalls == 0 || mockRangeDownloadCount(mock) == 0 {
		t.Fatalf("chunker read did not exercise ranged Degoo downloads: %+v", open)
	}
	t.Logf("encrypted range downloads so far: %d", mockRangeDownloadCount(mock))

	const rangeStart, rangeEnd = 117, 376
	before = snapshotLookupTrace(base, mock)
	rangeDownloadsBefore := mockRangeDownloadCount(mock)
	reader, err := coldObject.Open(ctx, &fs.RangeOption{Start: rangeStart, End: rangeEnd})
	if err != nil {
		t.Fatalf("open encrypted range: %v", err)
	}
	rangeData, readErr := io.ReadAll(reader)
	closeTestBody(t, reader)
	if readErr != nil {
		t.Fatalf("read encrypted range: %v", readErr)
	}
	if want := first[rangeStart : rangeEnd+1]; !bytes.Equal(rangeData, want) {
		t.Fatalf("decrypted range = %q, want the matching plaintext slice", rangeData)
	}
	if got := mockRangeDownloadCount(mock); got <= rangeDownloadsBefore {
		t.Fatalf("explicit range open issued no ranged Degoo requests: before=%d after=%d", rangeDownloadsBefore, got)
	}
	logLookupTrace(t, "encrypted explicit range open", lookupTraceDelta(before, snapshotLookupTrace(base, mock)))

	before = snapshotLookupTrace(base, mock)
	if got := readObject(t, warmObject); !bytes.Equal(got, first) {
		t.Fatalf("warm decrypted open differs from uploaded bytes")
	}
	warmOpen := lookupTraceDelta(before, snapshotLookupTrace(base, mock))
	logLookupTrace(t, "encrypted warm open", warmOpen)
	if warmOpen.downloadCalls == 0 || warmOpen.stats.MetadataPages != 0 {
		t.Fatalf("warm encrypted open trace = %+v, want payload reads without metadata pages", warmOpen)
	}

	base.DirCacheFlush()
	before = snapshotLookupTrace(base, mock)
	if _, err := stack.NewObject(ctx, name); err != nil {
		t.Fatalf("encrypted NewObject after refresh: %v", err)
	}
	refresh := lookupTraceDelta(before, snapshotLookupTrace(base, mock))
	logLookupTrace(t, "encrypted metadata refresh", refresh)
	if refresh.listCalls == 0 || refresh.stats.MetadataCacheMisses == 0 {
		t.Fatalf("encrypted lookup after refresh did not rebuild metadata: %+v", refresh)
	}

	replacement := bytes.Repeat([]byte("replacement-private-payload-"), 19)
	before = snapshotLookupTrace(base, mock)
	updated, err := stack.Put(ctx, bytes.NewReader(replacement), object.NewStaticObjectInfo(name, createdAt, int64(len(replacement)), true, nil, stack))
	if err != nil {
		t.Fatalf("chunker overwrite through crypt: %v", err)
	}
	overwriteTrace := lookupTraceDelta(before, snapshotLookupTrace(base, mock))
	logLookupTrace(t, "encrypted overwrite", overwriteTrace)
	if overwriteTrace.stats.RequestsByOperation["SetUploadFile3"] == 0 {
		t.Fatalf("encrypted overwrite omitted upload registration: %v", overwriteTrace.stats.RequestsByOperation)
	}
	assertEncryptedRemoteState(t, mock, first, replacement, []byte("generated-private-payload-"), []byte("replacement-private-payload-"))
	if got := readObject(t, updated); !bytes.Equal(got, replacement) {
		t.Fatalf("decrypted overwritten bytes differ from replacement")
	}
	t.Logf("encrypted overwrite: downloads=%d, operations=%v", overwriteTrace.downloadCalls, overwriteTrace.stats.RequestsByOperation)

	before = snapshotLookupTrace(base, mock)
	renamed, err := stack.Move(ctx, updated, "renamed-document.bin")
	if err != nil {
		t.Fatalf("encrypted chunker rename: %v", err)
	}
	renameTrace := lookupTraceDelta(before, snapshotLookupTrace(base, mock))
	logLookupTrace(t, "encrypted rename", renameTrace)
	if renameTrace.stats.RequestsByOperation["SetRenameFile"] == 0 {
		t.Fatalf("encrypted rename omitted Degoo rename requests: %v", renameTrace.stats.RequestsByOperation)
	}
	assertEncryptedRemoteState(t, mock, first, replacement, []byte("generated-private-payload-"), []byte("replacement-private-payload-"))
	if got := readObject(t, renamed); !bytes.Equal(got, replacement) {
		t.Fatalf("decrypted renamed bytes differ from replacement")
	}
	if _, err := stack.NewObject(ctx, name); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("NewObject at encrypted old path error = %v, want not found", err)
	}

	before = snapshotLookupTrace(base, mock)
	if err := renamed.Remove(ctx); err != nil {
		t.Fatalf("encrypted chunker delete: %v", err)
	}
	deleteTrace := lookupTraceDelta(before, snapshotLookupTrace(base, mock))
	logLookupTrace(t, "encrypted delete", deleteTrace)
	if deleteTrace.stats.RequestsByOperation["SetDeleteFile5"] == 0 {
		t.Fatalf("encrypted delete omitted Degoo delete requests: %v", deleteTrace.stats.RequestsByOperation)
	}
	if _, err := stack.NewObject(ctx, "renamed-document.bin"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("NewObject after encrypted delete error = %v, want not found", err)
	}
	if got := mockVaultDataObjectCount(mock); got != 0 {
		t.Fatalf("remaining generated encrypted Degoo objects after logical delete = %d, want 0", got)
	}
}
