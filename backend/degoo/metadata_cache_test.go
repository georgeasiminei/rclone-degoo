package degoo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
)

func TestMetadataCacheKeyTracksObjectVersion(t *testing.T) {
	modified := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	created := modified.Add(-time.Hour)
	key, ok := metadataCacheKey("opaque-item-id", 12, modified, created)
	if !ok {
		t.Fatal("metadataCacheKey rejected a versioned item")
	}
	changedTimeKey, ok := metadataCacheKey("opaque-item-id", 12, modified.Add(time.Second), created)
	if !ok || key == changedTimeKey {
		t.Fatal("equal-size item with changed modification time reused its cache key")
	}
	changedCreationKey, ok := metadataCacheKey("opaque-item-id", 12, modified, created.Add(time.Second))
	if !ok || key == changedCreationKey {
		t.Fatal("changed creation time reused its cache key")
	}
	changedSizeKey, ok := metadataCacheKey("opaque-item-id", 13, modified, created)
	if !ok || key == changedSizeKey {
		t.Fatal("changed size reused its cache key")
	}
	if _, ok := metadataCacheKey("opaque-item-id", 12, time.Time{}, time.Time{}); ok {
		t.Fatal("unversioned item received a cache key")
	}
}

func TestMetadataCachePersistsAndRejectsBadContent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "namespace")
	cache, err := newMetadataCache(dir)
	if err != nil {
		t.Fatalf("newMetadataCache: %v", err)
	}
	key := strings.Repeat("a", 64)
	content := []byte("generated encrypted object bytes")
	if err := cache.store(context.Background(), key, content); err != nil {
		t.Fatalf("store: %v", err)
	}
	cachePath := cache.filePath(key)
	serialized, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	cacheInfo, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat cache file: %v", err)
	}
	if cacheInfo.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode = %v, want 0600", cacheInfo.Mode().Perm())
	}
	if strings.Contains(string(serialized), "https://") || strings.Contains(string(serialized), "/download/") {
		t.Fatal("cache file contains a download URL")
	}

	reopened, err := newMetadataCache(dir)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	got, ok := reopened.read(key, int64(len(content)))
	if !ok || !bytes.Equal(got, content) {
		t.Fatalf("persistent cache read = %q, %t; want %q, true", got, ok, content)
	}

	corrupt := append([]byte(nil), serialized...)
	corrupt[len(corrupt)-1] ^= 0xff
	if err := os.WriteFile(cachePath, corrupt, 0o600); err != nil {
		t.Fatalf("corrupt cache file: %v", err)
	}
	if got, ok := reopened.read(key, int64(len(content))); ok {
		t.Fatalf("corrupt cache entry was accepted: %q", got)
	}

	if err := reopened.store(context.Background(), key, content); err != nil {
		t.Fatalf("rewrite cache: %v", err)
	}
	reopened.now = func() time.Time { return time.Now().Add(metadataCacheTTL + time.Second) }
	if got, ok := reopened.read(key, int64(len(content))); ok {
		t.Fatalf("expired cache entry was accepted: %q", got)
	}
}

func TestMetadataCacheRejectsUnsafeCacheFiles(t *testing.T) {
	cache, err := newMetadataCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("1", 64)
	content := []byte("encrypted metadata")
	if err := cache.store(context.Background(), key, content); err != nil {
		t.Fatal(err)
	}
	path := cache.filePath(key)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.read(key, int64(len(content))); ok {
		t.Fatal("cache accepted a group-readable file")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, ok := cache.read(key, int64(len(content))); ok {
		t.Fatal("cache accepted a symlink entry")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), metadataCacheMaxObjectSize+1025), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.read(key, int64(len(content))); ok {
		t.Fatal("cache accepted an oversized entry")
	}
}

func TestMetadataCacheDoesNotPersistPartialFill(t *testing.T) {
	cache, err := newMetadataCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("2", 64)
	if _, err := cache.open(context.Background(), key, 10, func(context.Context) ([]byte, error) {
		return []byte("partial"), nil
	}); err == nil {
		t.Fatal("partial fill returned without an error")
	}
	if _, err := os.Lstat(cache.filePath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial fill cache file stat error = %v, want not exist", err)
	}
}

func TestMetadataCacheRetainsMoreThan256SmallEntriesAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	cache, err := newMetadataCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	const count = 300
	keys := make([]string, count)
	contents := make([][]byte, count)
	for i := 0; i < count; i++ {
		keys[i] = fmt.Sprintf("%064x", i+1)
		contents[i] = []byte(fmt.Sprintf("encrypted-%04d", i))
		if err := cache.store(context.Background(), keys[i], contents[i]); err != nil {
			t.Fatalf("store entry %d: %v", i, err)
		}
	}
	restarted, err := newMetadataCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, count / 2, count - 1} {
		got, ok := restarted.read(keys[index], int64(len(contents[index])))
		if !ok || !bytes.Equal(got, contents[index]) {
			t.Fatalf("restarted entry %d = %q, %t", index, got, ok)
		}
	}
	if restarted.Snapshot().Evictions != 0 {
		t.Fatalf("evictions after restart = %d, want 0", restarted.Snapshot().Evictions)
	}
}

func TestMetadataCacheEnforcesEntryAndEncodedByteBounds(t *testing.T) {
	cache, err := newMetadataCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	cache.entries = make(map[string]metadataCacheEntry, metadataCacheMaxEntries)
	cache.totalBytes = 0
	for i := 0; i < metadataCacheMaxEntries; i++ {
		name := fmt.Sprintf("%064x.cache", i+1)
		cache.entries[name] = metadataCacheEntry{name: name, size: 1, modTime: time.Now().Add(-time.Duration(i) * time.Nanosecond)}
		cache.totalBytes++
	}
	cache.updateDirectoryModTime()
	cache.lastSweep = cache.now()
	if _, err := cache.evictLocked(context.Background(), 1, strings.Repeat("f", 64), false); err != nil {
		t.Fatalf("evict count bound: %v", err)
	}
	if len(cache.entries) >= metadataCacheMaxEntries {
		t.Fatalf("entry count after eviction = %d, want below %d", len(cache.entries), metadataCacheMaxEntries)
	}

	cache.entries = map[string]metadataCacheEntry{
		strings.Repeat("a", 64) + ".cache": {name: strings.Repeat("a", 64) + ".cache", size: metadataCacheMaxBytes, modTime: time.Now()},
	}
	cache.totalBytes = metadataCacheMaxBytes
	cache.updateDirectoryModTime()
	cache.lastSweep = cache.now()
	if _, err := cache.evictLocked(context.Background(), 1024, strings.Repeat("b", 64), false); err != nil {
		t.Fatalf("evict byte bound: %v", err)
	}
	if cache.totalBytes+1024 > metadataCacheMaxBytes {
		t.Fatalf("encoded bytes after eviction = %d, bound %d", cache.totalBytes+1024, metadataCacheMaxBytes)
	}
}

func TestMetadataCacheRejectsUnsafeDirectories(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "cache-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := newMetadataCache(link); err == nil {
		t.Fatal("metadata cache accepted a symlink directory")
	}

	public := filepath.Join(t.TempDir(), "public-cache")
	if err := os.Mkdir(public, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newMetadataCache(public); err == nil {
		t.Fatal("metadata cache accepted a directory accessible by group or other users")
	}
}

func TestMetadataCacheFillSingleflightAndWaiterCancellation(t *testing.T) {
	cache, err := newMetadataCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("b", 64)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context) ([]byte, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return []byte("encrypted"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterResult := make(chan error, 1)
	go func() {
		_, err := cache.open(waiterCtx, key, int64(len("encrypted")), load)
		waiterResult <- err
	}()
	<-started

	survivingResult := make(chan error, 1)
	go func() {
		data, err := cache.open(context.Background(), key, int64(len("encrypted")), load)
		if err == nil && string(data) != "encrypted" {
			err = errors.New("singleflight returned unexpected bytes")
		}
		survivingResult <- err
	}()
	cancelWaiter()
	if err := <-waiterResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context canceled", err)
	}
	close(release)
	if err := <-survivingResult; err != nil {
		t.Fatalf("surviving waiter: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("loader calls = %d, want 1", got)
	}
}

func TestMetadataCacheClearForcesRefillAndInvalidatesInflightFill(t *testing.T) {
	cache, err := newMetadataCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("d", 64)
	oldContent := []byte("old encrypted metadata")
	newContent := []byte("new encrypted metadata")
	if len(oldContent) != len(newContent) {
		t.Fatal("test contents must have equal size")
	}
	if err := cache.store(context.Background(), key, oldContent); err != nil {
		t.Fatal(err)
	}
	if err := cache.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	got, err := cache.open(context.Background(), key, int64(len(newContent)), func(context.Context) ([]byte, error) {
		return newContent, nil
	})
	if err != nil || !bytes.Equal(got, newContent) {
		t.Fatalf("open after Clear = %q, %v; want new content", got, err)
	}

	inflightKey := strings.Repeat("e", 64)
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		data, err := cache.open(context.Background(), inflightKey, int64(len(oldContent)), func(context.Context) ([]byte, error) {
			close(started)
			<-release
			return oldContent, nil
		})
		if err == nil && !bytes.Equal(data, oldContent) {
			err = errors.New("in-flight fill returned unexpected content")
		}
		result <- err
	}()
	<-started
	if err := cache.Clear(); err != nil {
		t.Fatalf("Clear during fill: %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("in-flight Open: %v", err)
	}
	if _, ok := cache.read(inflightKey, int64(len(oldContent))); ok {
		t.Fatal("in-flight fill repopulated the cache after Clear")
	}
}

func TestMetadataCacheRangesReuseFullObjectAcrossCacheInstances(t *testing.T) {
	mock := newMockDegoo()
	content := []byte("generated encrypted manifest payload")
	mock.items["metadata-payload"] = mockItem{
		id: "metadata-payload", name: "encrypted-manifest", parentID: "vault", category: 6,
		size: int64(len(content)), created: time.Now().Add(-time.Minute), content: content,
	}
	f, server := newTestFs(t, mock)
	defer server.Close()
	cacheDir := filepath.Join(t.TempDir(), "bound-namespace")
	if err := f.EnableMetadataCache(cacheDir); err != nil {
		t.Fatalf("EnableMetadataCache: %v", err)
	}
	obj, err := f.NewObject(context.Background(), "encrypted-manifest")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	key, ok := metadataCacheKey(obj.(*Object).id, obj.Size(), obj.ModTime(context.Background()), obj.(*Object).createdTime)
	if !ok {
		t.Fatal("test object has no versioned cache key")
	}
	if err := os.WriteFile(f.metadataCache.filePath(key), []byte("truncated"), 0o600); err != nil {
		t.Fatalf("write malformed cache entry: %v", err)
	}
	body, err := obj.Open(context.Background())
	if err != nil {
		t.Fatalf("full Open: %v", err)
	}
	got, readErr := io.ReadAll(body)
	closeTestBody(t, body)
	if readErr != nil || !bytes.Equal(got, content) {
		t.Fatalf("full cached Open = %q, read error %v", got, readErr)
	}

	body, err = obj.Open(context.Background(), &fs.RangeOption{Start: 4, End: 11})
	if err != nil {
		t.Fatalf("range Open: %v", err)
	}
	got, readErr = io.ReadAll(body)
	closeTestBody(t, body)
	if readErr != nil || !bytes.Equal(got, content[4:12]) {
		t.Fatalf("range cached Open = %q, read error %v", got, readErr)
	}

	changedContent := []byte("recreated encrypted manifest payload")
	if len(changedContent) != len(content) {
		t.Fatalf("test content lengths differ: %d and %d", len(changedContent), len(content))
	}
	changedObject := obj.(*Object).snapshot()
	changedObject.modTime = changedObject.modTime.Add(time.Second)
	changedObject.createdTime = changedObject.createdTime.Add(time.Second)
	mock.mu.Lock()
	changedItem := mock.items["metadata-payload"]
	changedItem.content = changedContent
	changedItem.created = changedItem.created.Add(time.Second)
	mock.items["metadata-payload"] = changedItem
	mock.mu.Unlock()
	body, err = changedObject.Open(context.Background())
	if err != nil {
		t.Fatalf("Open after equal-size version change: %v", err)
	}
	got, readErr = io.ReadAll(body)
	closeTestBody(t, body)
	if readErr != nil || !bytes.Equal(got, changedContent) {
		t.Fatalf("changed-version Open = %q, read error %v", got, readErr)
	}
	originalStats := f.MetadataCacheStats()
	if originalStats.Fills < 2 {
		t.Fatalf("cache stats before restart = %+v, want both object versions filled", originalStats)
	}

	cache, err := newMetadataCache(cacheDir)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	f.metadataCache = cache
	mock.mu.Lock()
	beforeRestartHit := mock.downloadCalls
	mock.mu.Unlock()
	body, err = obj.Open(context.Background(), &fs.RangeOption{Start: 0, End: 3})
	if err != nil {
		t.Fatalf("range Open after cache restart: %v", err)
	}
	got, readErr = io.ReadAll(body)
	closeTestBody(t, body)
	if readErr != nil || !bytes.Equal(got, content[:4]) {
		t.Fatalf("reopened cache range = %q, read error %v", got, readErr)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.downloadCalls != beforeRestartHit {
		t.Fatalf("downloads = %d, want no additional request after reopening the cache", mock.downloadCalls)
	}
	stats := f.MetadataCacheStats()
	if stats.Hits < 1 {
		t.Fatalf("cache stats after restart = %+v, want a hit", stats)
	}
}

func TestMetadataCacheOversizedObjectUsesRemote(t *testing.T) {
	mock := newMockDegoo()
	content := bytes.Repeat([]byte("x"), metadataCacheMaxObjectSize+1)
	created := time.Now().Add(-time.Minute)
	mock.items["large-payload"] = mockItem{
		id: "large-payload", name: "large-object", parentID: "vault", category: 6,
		size: int64(len(content)), created: created, content: content,
	}
	f, server := newTestFs(t, mock)
	defer server.Close()
	if err := f.EnableMetadataCache(filepath.Join(t.TempDir(), "bound-namespace")); err != nil {
		t.Fatalf("EnableMetadataCache: %v", err)
	}
	obj, err := f.NewObject(context.Background(), "large-object")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	for i := 0; i < 2; i++ {
		body, err := obj.Open(context.Background())
		if err != nil {
			t.Fatalf("oversized Open %d: %v", i, err)
		}
		got, readErr := io.ReadAll(body)
		closeTestBody(t, body)
		if readErr != nil || !bytes.Equal(got, content) {
			t.Fatalf("oversized Open %d read error %v", i, readErr)
		}
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.downloadCalls != 2 {
		t.Fatalf("oversized object downloads = %d, want 2", mock.downloadCalls)
	}
}

type requiredUnknownOpenOption struct{}

func (requiredUnknownOpenOption) String() string           { return "required unknown option" }
func (requiredUnknownOpenOption) Header() (string, string) { return "", "" }
func (requiredUnknownOpenOption) Mandatory() bool          { return true }

func TestMetadataCacheRejectsUnknownMandatoryOpenOption(t *testing.T) {
	mock := newMockDegoo()
	content := []byte("small encrypted item")
	mock.items["payload"] = mockItem{
		id: "payload", name: "encrypted", parentID: "vault", category: 6,
		size: int64(len(content)), created: time.Now().Add(-time.Minute), content: content,
	}
	f, server := newTestFs(t, mock)
	defer server.Close()
	if err := f.EnableMetadataCache(filepath.Join(t.TempDir(), "namespace")); err != nil {
		t.Fatalf("EnableMetadataCache: %v", err)
	}
	obj, err := f.NewObject(context.Background(), "encrypted")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if _, err := obj.Open(context.Background(), requiredUnknownOpenOption{}); err == nil || !strings.Contains(err.Error(), "unsupported mandatory open option") {
		t.Fatalf("Open with unknown mandatory option error = %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.downloadCalls != 0 {
		t.Fatalf("downloads = %d, want no request for an unsupported mandatory option", mock.downloadCalls)
	}
}

func TestMetadataCacheReadHonorsCancellation(t *testing.T) {
	cache, err := newMetadataCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("c", 64)
	content := []byte("generated ciphertext")
	if err := cache.store(context.Background(), key, content); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.open(ctx, key, int64(len(content)), func(context.Context) ([]byte, error) {
		return nil, errors.New("loader should not run")
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cache open error = %v, want context canceled", err)
	}
	reader := &metadataCacheReadCloser{ctx: ctx, reader: bytes.NewReader(content)}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached read error = %v, want context canceled", err)
	}
}
