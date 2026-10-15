package degoo

import (
	"bufio"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/lib/dircache"
)

// persistentLocalIDIndex is a test-only model of a bounded local name-to-ID
// cache over the current layout. Remote listings remain authoritative: a cache
// miss rebuilds from every listing page, and a hit uses GetItem for fresh data.
// It is deliberately not connected to the production backend API.
type persistentLocalIDIndex struct {
	mu         sync.Mutex
	dir        string
	maxEntries int
	maxBytes   int64
	generation uint64
	usedBytes  int64
	entries    map[string]*list.Element
	order      list.List
	stats      localIDIndexStats
}

const localIDIndexFilename = "id-index.jsonl"

var errLocalIDIndexChanged = errors.New("local ID index generation changed during lookup")

type localIDIndexHeader struct {
	Format     int    `json:"format"`
	Generation uint64 `json:"generation"`
}

type localIDIndexRecord struct {
	Generation uint64 `json:"generation"`
	ParentID   string `json:"parent_id"`
	Name       string `json:"name"`
	ID         string `json:"id"`
}

type localIDIndexStats struct {
	Lookups               uint64 `json:"lookups"`
	Hits                  uint64 `json:"hits"`
	Misses                uint64 `json:"misses"`
	Builds                uint64 `json:"builds"`
	EnumeratedEntries     uint64 `json:"enumerated_entries"`
	EvictedEntries        uint64 `json:"evicted_entries"`
	MutationInvalidations uint64 `json:"mutation_invalidations"`
	RefreshInvalidations  uint64 `json:"refresh_invalidations"`
}

func localIDIndexKey(parentID, name string) string { return parentID + "\x00" + name }

func localIDIndexPath(dir string) string { return filepath.Join(dir, localIDIndexFilename) }

func openPersistentLocalIDIndex(dir string, maxEntries int, maxBytes int64) (*persistentLocalIDIndex, error) {
	if maxEntries <= 0 || maxBytes <= 0 {
		return nil, fmt.Errorf("invalid local index limits: entries=%d bytes=%d", maxEntries, maxBytes)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	index := &persistentLocalIDIndex{
		dir: dir, maxEntries: maxEntries, maxBytes: maxBytes,
		generation: 1, entries: make(map[string]*list.Element),
	}
	if err := index.load(); err != nil {
		return nil, err
	}
	return index, nil
}

func (index *persistentLocalIDIndex) headerBytes() (int64, error) {
	data, err := json.Marshal(localIDIndexHeader{Format: 1, Generation: index.generation})
	return int64(len(data) + 1), err
}

func (index *persistentLocalIDIndex) load() error {
	path := localIDIndexPath(index.dir)
	if err := os.Remove(path + ".tmp"); err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		index.usedBytes, err = index.headerBytes()
		return err
	}
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > index.maxBytes {
		return index.discardFile()
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1<<20)
	if !scanner.Scan() {
		return index.discardFile()
	}
	var header localIDIndexHeader
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil || header.Format != 1 || header.Generation == 0 {
		return index.discardFile()
	}
	index.generation = header.Generation
	index.usedBytes = int64(len(scanner.Bytes()) + 1)
	for scanner.Scan() {
		if len(index.entries) >= index.maxEntries {
			return index.discardFile()
		}
		line := append([]byte(nil), scanner.Bytes()...)
		var record localIDIndexRecord
		if err := json.Unmarshal(line, &record); err != nil || record.Generation != index.generation || record.ParentID == "" || record.Name == "" || record.ID == "" {
			return index.discardFile()
		}
		key := localIDIndexKey(record.ParentID, record.Name)
		if _, duplicate := index.entries[key]; duplicate {
			return index.discardFile()
		}
		index.entries[key] = index.order.PushBack(record)
		index.usedBytes += int64(len(line) + 1)
	}
	if err := scanner.Err(); err != nil {
		return index.discardFile()
	}
	if len(index.entries) > index.maxEntries || index.usedBytes > index.maxBytes {
		return index.discardFile()
	}
	return nil
}

func (index *persistentLocalIDIndex) discardFile() error {
	if err := os.Remove(localIDIndexPath(index.dir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	index.entries = make(map[string]*list.Element)
	index.order.Init()
	returnBytes, err := index.headerBytes()
	index.usedBytes = returnBytes
	return err
}

func (index *persistentLocalIDIndex) recordLineBytes(record localIDIndexRecord) (int64, error) {
	data, err := json.Marshal(record)
	return int64(len(data) + 1), err
}

func (index *persistentLocalIDIndex) add(record localIDIndexRecord) error {
	record.Generation = index.generation
	lineBytes, err := index.recordLineBytes(record)
	if err != nil {
		return err
	}
	headerBytes, err := index.headerBytes()
	if err != nil {
		return err
	}
	if lineBytes+headerBytes > index.maxBytes {
		return nil
	}
	key := localIDIndexKey(record.ParentID, record.Name)
	if previous := index.entries[key]; previous != nil {
		index.usedBytes -= index.elementBytes(previous)
		index.order.Remove(previous)
		delete(index.entries, key)
	}
	for len(index.entries) >= index.maxEntries || index.usedBytes+lineBytes > index.maxBytes {
		oldest := index.order.Front()
		if oldest == nil {
			break
		}
		oldRecord := oldest.Value.(localIDIndexRecord)
		index.usedBytes -= index.elementBytes(oldest)
		delete(index.entries, localIDIndexKey(oldRecord.ParentID, oldRecord.Name))
		index.order.Remove(oldest)
		index.stats.EvictedEntries++
	}
	index.entries[key] = index.order.PushBack(record)
	index.usedBytes += lineBytes
	return nil
}

func (index *persistentLocalIDIndex) elementBytes(element *list.Element) int64 {
	bytes, _ := index.recordLineBytes(element.Value.(localIDIndexRecord))
	return bytes
}

func (index *persistentLocalIDIndex) save() error {
	path := localIDIndexPath(index.dir)
	temp := path + ".tmp"
	// The cache is disposable. Removing the previous copy before writing the
	// replacement keeps the directory's on-disk byte bound meaningful even
	// while the bounded temporary file is being written.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(temp); err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	writeErr := func() error {
		writer := bufio.NewWriter(file)
		if err := writeLocalIDIndexLine(writer, localIDIndexHeader{Format: 1, Generation: index.generation}); err != nil {
			return err
		}
		for element := index.order.Front(); element != nil; element = element.Next() {
			if err := writeLocalIDIndexLine(writer, element.Value.(localIDIndexRecord)); err != nil {
				return err
			}
		}
		return writer.Flush()
	}()
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(temp)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(temp)
		return closeErr
	}
	info, err := os.Stat(temp)
	if err != nil {
		_ = os.Remove(temp)
		return err
	}
	if info.Size() > index.maxBytes || int64(len(index.entries)) > int64(index.maxEntries) {
		_ = os.Remove(temp)
		return fmt.Errorf("local index exceeds configured limits: entries=%d bytes=%d", len(index.entries), info.Size())
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	index.usedBytes = info.Size()
	return nil
}

func writeLocalIDIndexLine(writer *bufio.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		return err
	}
	return writer.WriteByte('\n')
}

func (index *persistentLocalIDIndex) lookup(ctx context.Context, f *Fs, parentID, name string) (api.Item, bool, error) {
	key := localIDIndexKey(parentID, name)
	index.mu.Lock()
	index.stats.Lookups++
	generation := index.generation
	element := index.entries[key]
	if element != nil {
		index.stats.Hits++
		record := element.Value.(localIDIndexRecord)
		index.mu.Unlock()
		item, err := f.api.GetItem(ctx, record.ID)
		index.mu.Lock()
		changed := index.generation != generation
		index.mu.Unlock()
		if changed {
			return api.Item{}, true, errLocalIDIndexChanged
		}
		if err != nil {
			return api.Item{}, true, err
		}
		if item.ID != record.ID || item.Name != name || item.ParentID != parentID {
			return api.Item{}, true, fmt.Errorf("direct item identity did not match cached index record")
		}
		return item, true, nil
	}
	index.stats.Misses++
	index.mu.Unlock()

	// The current layout remains the source of truth. A cold or evicted local
	// key requires a complete paginated scan before any mapping can be trusted.
	items, err := f.api.ListDirFresh(ctx, parentID)
	if err != nil {
		return api.Item{}, false, err
	}
	var found api.Item
	foundSet := false
	index.mu.Lock()
	if index.generation != generation {
		index.mu.Unlock()
		return api.Item{}, false, errLocalIDIndexChanged
	}
	index.stats.Builds++
	index.stats.EnumeratedEntries += uint64(len(items))
	index.entries = make(map[string]*list.Element)
	index.order.Init()
	index.usedBytes, err = index.headerBytes()
	if err == nil {
		for _, item := range items {
			if item.ParentID != parentID {
				continue
			}
			if item.Name == name {
				found = item
				foundSet = true
			}
			if err := index.add(localIDIndexRecord{ParentID: item.ParentID, Name: item.Name, ID: item.ID}); err != nil {
				index.mu.Unlock()
				return api.Item{}, false, err
			}
		}
		if err = index.save(); err != nil {
			index.mu.Unlock()
			return api.Item{}, false, err
		}
	}
	index.mu.Unlock()
	if err != nil {
		return api.Item{}, false, err
	}
	if !foundSet {
		return api.Item{}, false, fmt.Errorf("item %q was absent from the complete generated listing", name)
	}
	return found, false, nil
}

func (index *persistentLocalIDIndex) build(parentID string, items []api.Item) error {
	index.mu.Lock()
	defer index.mu.Unlock()
	index.stats.Builds++
	index.stats.EnumeratedEntries += uint64(len(items))
	index.entries = make(map[string]*list.Element)
	index.order.Init()
	index.usedBytes, _ = index.headerBytes()
	for _, item := range items {
		if item.ParentID != parentID {
			continue
		}
		if err := index.add(localIDIndexRecord{ParentID: item.ParentID, Name: item.Name, ID: item.ID}); err != nil {
			return err
		}
	}
	return index.save()
}

func (index *persistentLocalIDIndex) invalidateMutation() error {
	return index.invalidate(true)
}

func (index *persistentLocalIDIndex) invalidateRefresh() error {
	return index.invalidate(false)
}

func (index *persistentLocalIDIndex) invalidate(mutation bool) error {
	index.mu.Lock()
	defer index.mu.Unlock()
	index.generation++
	if mutation {
		index.stats.MutationInvalidations++
	} else {
		index.stats.RefreshInvalidations++
	}
	index.entries = make(map[string]*list.Element)
	index.order.Init()
	var err error
	index.usedBytes, err = index.headerBytes()
	if err != nil {
		return err
	}
	return index.save()
}

func (index *persistentLocalIDIndex) statsSnapshot() localIDIndexStats {
	index.mu.Lock()
	defer index.mu.Unlock()
	return index.stats
}

func (index *persistentLocalIDIndex) entryCount() int {
	index.mu.Lock()
	defer index.mu.Unlock()
	return len(index.entries)
}

func (index *persistentLocalIDIndex) generationNumber() uint64 {
	index.mu.Lock()
	defer index.mu.Unlock()
	return index.generation
}

func localIndexDiskBytes(t *testing.T, dir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

type localIndexMeasurement struct {
	Entries       int               `json:"entries"`
	Operation     string            `json:"operation"`
	DurationNS    int64             `json:"duration_ns"`
	Requests      uint64            `json:"requests"`
	Pages         uint64            `json:"pages"`
	DirectIDReads uint64            `json:"direct_id_reads"`
	ResponseBytes uint64            `json:"response_bytes"`
	IndexEntries  int               `json:"index_entries"`
	IndexBytes    int64             `json:"index_bytes"`
	Generation    uint64            `json:"generation"`
	IndexStats    localIDIndexStats `json:"index_stats"`
}

func measureLocalIndexLookup(t *testing.T, f *Fs, fixture *scalabilityFixture, index *persistentLocalIDIndex, entries int, operation string, wantPages, wantDirect uint64) localIndexMeasurement {
	t.Helper()
	beforeAPI := f.api.RequestStats()
	beforeBytes := fixture.bytes.Load()
	beforePages := fixture.pages.Load()
	beforeOverlays := fixture.overlays.Load()
	beforeStats := index.statsSnapshot()
	started := time.Now()
	item, direct, err := index.lookup(context.Background(), f, "vault", fmt.Sprintf("object-%08d", entries-1))
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("%s lookup: %v", operation, err)
	}
	if item.ID != fmt.Sprintf("opaque-%08d", entries-1) || item.Name != fmt.Sprintf("object-%08d", entries-1) {
		t.Fatalf("%s returned wrong generated item: %+v", operation, item)
	}
	if direct != (wantDirect == 1) {
		t.Fatalf("%s direct route=%t, want direct=%t", operation, direct, wantDirect == 1)
	}
	afterAPI := f.api.RequestStats()
	pageDelta := fixture.pages.Load() - beforePages
	directDelta := fixture.overlays.Load() - beforeOverlays
	requests := afterAPI.HTTPRequests - beforeAPI.HTTPRequests
	responseBytes := fixture.bytes.Load() - beforeBytes
	if pageDelta != wantPages || directDelta != wantDirect {
		t.Fatalf("%s requests: pages=%d direct=%d, want %d/%d", operation, pageDelta, directDelta, wantPages, wantDirect)
	}
	if requests != wantPages+wantDirect {
		t.Fatalf("%s HTTP requests=%d, want pages + direct reads = %d", operation, requests, wantPages+wantDirect)
	}
	stats := index.statsSnapshot()
	measurement := localIndexMeasurement{
		Entries: entries, Operation: operation, DurationNS: elapsed.Nanoseconds(), Requests: requests,
		Pages: pageDelta, DirectIDReads: directDelta, ResponseBytes: responseBytes,
		IndexEntries: index.entryCount(), IndexBytes: localIndexDiskBytes(t, index.dir),
		Generation: index.generationNumber(),
		IndexStats: localIDIndexStats{
			Lookups: stats.Lookups - beforeStats.Lookups, Hits: stats.Hits - beforeStats.Hits,
			Misses: stats.Misses - beforeStats.Misses, Builds: stats.Builds - beforeStats.Builds,
			EnumeratedEntries:     stats.EnumeratedEntries - beforeStats.EnumeratedEntries,
			EvictedEntries:        stats.EvictedEntries - beforeStats.EvictedEntries,
			MutationInvalidations: stats.MutationInvalidations - beforeStats.MutationInvalidations,
			RefreshInvalidations:  stats.RefreshInvalidations - beforeStats.RefreshInvalidations,
		},
	}
	encoded, err := json.Marshal(measurement)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
	return measurement
}

func TestScalabilityPersistentLocalIndex(t *testing.T) {
	for _, count := range []int{1000, 10000, 100000} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			if count == 100000 && os.Getenv("RCLONE_DEGOO_SCALE_TESTS") != "1" {
				t.Skip("set RCLONE_DEGOO_SCALE_TESTS=1 for the 100,000-entry local-index measurement")
			}
			f, fixture := newScalabilityFs(t, count, 1000)
			cacheDir := t.TempDir()
			const maxEntries = 512
			const maxBytes = 16 * 1024
			index, err := openPersistentLocalIDIndex(cacheDir, maxEntries, maxBytes)
			if err != nil {
				t.Fatal(err)
			}
			pages := uint64((count + fixture.pageSize - 1) / fixture.pageSize)
			build := measureLocalIndexLookup(t, f, fixture, index, count, "cold_build", pages, 0)
			if build.IndexStats.Builds != 1 || build.IndexStats.EnumeratedEntries != uint64(count) || build.IndexStats.Misses != 1 {
				t.Fatalf("cold build stats = %+v", build.IndexStats)
			}
			if build.IndexEntries > maxEntries || build.IndexBytes > maxBytes {
				t.Fatalf("local index exceeded limits: entries=%d/%d bytes=%d/%d", build.IndexEntries, maxEntries, build.IndexBytes, maxBytes)
			}

			// Reopening exercises the actual JSONL record on disk. A warm hit
			// retrieves fresh metadata through the fixture's direct-ID endpoint.
			index, err = openPersistentLocalIDIndex(cacheDir, maxEntries, maxBytes)
			if err != nil {
				t.Fatal(err)
			}
			warm := measureLocalIndexLookup(t, f, fixture, index, count, "restart_warm_direct_id", 0, 1)
			if warm.IndexStats.Hits != 1 || warm.IndexStats.Misses != 0 || warm.IndexStats.Builds != 0 {
				t.Fatalf("restart warm stats = %+v", warm.IndexStats)
			}
		})
	}
}

func TestScalabilityPersistentLocalIndexBounds(t *testing.T) {
	items := make([]api.Item, 10)
	for index := range items {
		items[index] = api.Item{
			ID: fmt.Sprintf("opaque-%08d", index), Name: fmt.Sprintf("object-%08d", index),
			ParentID: "vault", Category: 6, Size: 4096,
		}
	}

	t.Run("entry_limit", func(t *testing.T) {
		index, err := openPersistentLocalIDIndex(t.TempDir(), 2, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if err := index.build("vault", items); err != nil {
			t.Fatal(err)
		}
		if got := index.entryCount(); got != 2 {
			t.Fatalf("entry count=%d, want 2", got)
		}
		if got := localIndexDiskBytes(t, index.dir); got > index.maxBytes {
			t.Fatalf("disk bytes=%d, max %d", got, index.maxBytes)
		}
		if index.statsSnapshot().EvictedEntries != 8 {
			t.Fatalf("evicted entries=%d, want 8", index.statsSnapshot().EvictedEntries)
		}
	})

	t.Run("byte_limit", func(t *testing.T) {
		probe, err := openPersistentLocalIDIndex(t.TempDir(), 100, 4096)
		if err != nil {
			t.Fatal(err)
		}
		headerBytes, err := probe.headerBytes()
		if err != nil {
			t.Fatal(err)
		}
		lineBytes, err := probe.recordLineBytes(localIDIndexRecord{
			Generation: 1, ParentID: "vault", Name: "object-00000000", ID: "opaque-00000000",
		})
		if err != nil {
			t.Fatal(err)
		}
		maxBytes := headerBytes + 3*lineBytes
		index, err := openPersistentLocalIDIndex(t.TempDir(), 100, maxBytes)
		if err != nil {
			t.Fatal(err)
		}
		if err := index.build("vault", items); err != nil {
			t.Fatal(err)
		}
		if got := index.entryCount(); got != 3 {
			t.Fatalf("entry count=%d, want 3 under byte cap", got)
		}
		if got := localIndexDiskBytes(t, index.dir); got > maxBytes {
			t.Fatalf("disk bytes=%d, max %d", got, maxBytes)
		}
		if index.statsSnapshot().EvictedEntries != 7 {
			t.Fatalf("evicted entries=%d, want 7", index.statsSnapshot().EvictedEntries)
		}
	})
}

func TestScalabilityPersistentLocalIndexInvalidation(t *testing.T) {
	f, fixture := newScalabilityFs(t, 1000, 1000)
	index, err := openPersistentLocalIDIndex(t.TempDir(), 32, 4096)
	if err != nil {
		t.Fatal(err)
	}
	initial := measureLocalIndexLookup(t, f, fixture, index, 1000, "initial_build", 1, 0)
	if initial.IndexStats.Builds != 1 {
		t.Fatalf("initial build stats = %+v", initial.IndexStats)
	}

	initialGeneration := index.generationNumber()
	if err := index.invalidateMutation(); err != nil {
		t.Fatal(err)
	}
	if got := index.generationNumber(); got != initialGeneration+1 || index.entryCount() != 0 {
		t.Fatalf("after mutation invalidation generation/entries=%d/%d", got, index.entryCount())
	}
	mutation := measureLocalIndexLookup(t, f, fixture, index, 1000, "lookup_after_mutation", 1, 0)
	if mutation.IndexStats.Builds != 1 || mutation.IndexStats.MutationInvalidations != 0 {
		t.Fatalf("post-mutation lookup stats = %+v", mutation.IndexStats)
	}

	if err := index.invalidateRefresh(); err != nil {
		t.Fatal(err)
	}
	refresh := measureLocalIndexLookup(t, f, fixture, index, 1000, "lookup_after_refresh", 1, 0)
	if refresh.IndexStats.Builds != 1 || refresh.IndexStats.RefreshInvalidations != 0 {
		t.Fatalf("post-refresh lookup stats = %+v", refresh.IndexStats)
	}
	stats := index.statsSnapshot()
	if stats.MutationInvalidations != 1 || stats.RefreshInvalidations != 1 {
		t.Fatalf("invalidation counters=%+v", stats)
	}
}

func TestScalabilityPersistentLocalIndexMissingOrCorruptFallback(t *testing.T) {
	for _, corruption := range []string{"missing", "corrupt"} {
		t.Run(corruption, func(t *testing.T) {
			f, fixture := newScalabilityFs(t, 1000, 1000)
			cacheDir := t.TempDir()
			index, err := openPersistentLocalIDIndex(cacheDir, 64, 8192)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := index.lookup(context.Background(), f, "vault", "object-00000999"); err != nil {
				t.Fatal(err)
			}
			path := localIDIndexPath(cacheDir)
			if corruption == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("{broken record\n"), 0600); err != nil {
				t.Fatal(err)
			}

			index, err = openPersistentLocalIDIndex(cacheDir, 64, 8192)
			if err != nil {
				t.Fatal(err)
			}
			beforePages := fixture.pages.Load()
			beforeOverlays := fixture.overlays.Load()
			item, direct, err := index.lookup(context.Background(), f, "vault", "object-00000999")
			if err != nil {
				t.Fatal(err)
			}
			if direct || item.ID != "opaque-00000999" {
				t.Fatalf("fallback item/direct=%+v/%t", item, direct)
			}
			if got := fixture.pages.Load() - beforePages; got != 1 {
				t.Fatalf("fallback listing pages=%d, want full one-page rebuild", got)
			}
			if got := fixture.overlays.Load() - beforeOverlays; got != 0 {
				t.Fatalf("fallback direct-ID reads=%d, want 0", got)
			}
			stats := index.statsSnapshot()
			if stats.Misses != 1 || stats.Builds != 1 || stats.EnumeratedEntries != 1000 {
				t.Fatalf("fallback stats=%+v", stats)
			}
		})
	}
}

func TestScalabilityPersistentLocalIndexDoesNotStoreURLs(t *testing.T) {
	index, err := openPersistentLocalIDIndex(t.TempDir(), 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	item := api.Item{ID: "opaque-00000001", Name: "object-00000001", ParentID: "vault", URL: "https://ephemeral.invalid/download"}
	if err := index.build("vault", []api.Item{item}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(localIDIndexPath(index.dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ephemeral.invalid") {
		t.Fatal("local ID index persisted a short-lived download URL")
	}
}

type localIndexRequestGate struct {
	started chan struct{}
	release chan struct{}
}

type gatedScalabilityFixture struct {
	fixture *scalabilityFixture
	gates   chan *localIndexRequestGate
}

func (handler *gatedScalabilityFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	select {
	case gate := <-handler.gates:
		close(gate.started)
		<-gate.release
	default:
	}
	handler.fixture.ServeHTTP(writer, request)
}

func (handler *gatedScalabilityFixture) blockNextRequest() *localIndexRequestGate {
	gate := &localIndexRequestGate{started: make(chan struct{}), release: make(chan struct{})}
	handler.gates <- gate
	return gate
}

func newGatedScalabilityFs(t *testing.T, entries, pageSize int) (*Fs, *scalabilityFixture, *gatedScalabilityFixture) {
	t.Helper()
	fixture := &scalabilityFixture{entries: entries, pageSize: pageSize}
	handler := &gatedScalabilityFixture{fixture: fixture, gates: make(chan *localIndexRequestGate, 1)}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f := &Fs{
		name: "generated", opt: Options{RootFolderID: "vault"},
		api: api.New(server.Client(), scalabilityTokens{}, server.URL, "generated-key"),
	}
	f.dirCache = dircache.New("", "vault", f)
	return f, fixture, handler
}

func waitForLocalIndexGate(t *testing.T, gate *localIndexRequestGate) {
	t.Helper()
	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("generated API request did not reach its gate")
	}
}

func TestScalabilityPersistentLocalIndexRejectsLookupsAcrossInvalidation(t *testing.T) {
	f, _, handler := newGatedScalabilityFs(t, 1000, 1000)
	index, err := openPersistentLocalIDIndex(t.TempDir(), 64, 8192)
	if err != nil {
		t.Fatal(err)
	}

	listGate := handler.blockNextRequest()
	lookupResult := make(chan error, 1)
	go func() {
		_, _, err := index.lookup(context.Background(), f, "vault", "object-00000999")
		lookupResult <- err
	}()
	waitForLocalIndexGate(t, listGate)
	if err := index.invalidateMutation(); err != nil {
		t.Fatal(err)
	}
	close(listGate.release)
	if err := <-lookupResult; !errors.Is(err, errLocalIDIndexChanged) {
		t.Fatalf("lookup crossing mutation returned %v, want generation-change error", err)
	}
	if index.entryCount() != 0 || index.generationNumber() != 2 {
		t.Fatalf("listing repopulated stale state: entries=%d generation=%d", index.entryCount(), index.generationNumber())
	}
	if _, direct, err := index.lookup(context.Background(), f, "vault", "object-00000999"); err != nil || direct {
		t.Fatalf("lookup after mutation: direct=%t err=%v, want complete listing rebuild", direct, err)
	}

	directGate := handler.blockNextRequest()
	lookupResult = make(chan error, 1)
	go func() {
		_, _, err := index.lookup(context.Background(), f, "vault", "object-00000999")
		lookupResult <- err
	}()
	waitForLocalIndexGate(t, directGate)
	if err := index.invalidateRefresh(); err != nil {
		t.Fatal(err)
	}
	close(directGate.release)
	if err := <-lookupResult; !errors.Is(err, errLocalIDIndexChanged) {
		t.Fatalf("direct lookup crossing refresh returned %v, want generation-change error", err)
	}
	if index.entryCount() != 0 || index.generationNumber() != 3 {
		t.Fatalf("direct lookup repopulated stale state: entries=%d generation=%d", index.entryCount(), index.generationNumber())
	}
}
