package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"unsafe"
)

type scalabilityOccupancyTokens struct{}

func (scalabilityOccupancyTokens) Token(context.Context) (string, error) {
	return "generated-token", nil
}
func (scalabilityOccupancyTokens) Refresh(context.Context, string) (string, error) {
	return "generated-token", nil
}

type scalabilityOccupancyFixture struct {
	entries  int
	pageSize int
	pages    atomic.Uint64
	children atomic.Uint64
}

func scalabilityOccupancyID(parent string, index int) string {
	return fmt.Sprintf("memory-id-%s-%08d", parent, index)
}

func scalabilityOccupancyName(parent string, index int) string {
	return fmt.Sprintf("memory-name-%s-%08d", parent, index)
}

func (fixture *scalabilityOccupancyFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Operation string `json:"operationName"`
		Variables struct {
			ParentID  string
			Limit     int
			NextToken string
		} `json:"variables"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		http.Error(writer, "invalid fixture request", http.StatusBadRequest)
		return
	}
	if input.Operation != "GetFileChildren5" || input.Variables.ParentID == "" || input.Variables.Limit != 1000 {
		http.Error(writer, "unexpected fixture request", http.StatusBadRequest)
		return
	}
	offset := 0
	if input.Variables.NextToken != "" {
		var err error
		offset, err = strconv.Atoi(input.Variables.NextToken)
		if err != nil || offset <= 0 || offset >= fixture.entries || offset%fixture.pageSize != 0 {
			http.Error(writer, "invalid fixture cursor", http.StatusBadRequest)
			return
		}
	}
	end := min(offset+fixture.pageSize, fixture.entries)
	items := make([]map[string]any, 0, end-offset)
	for index := offset; index < end; index++ {
		items = append(items, map[string]any{
			"ID": scalabilityOccupancyID(input.Variables.ParentID, index), "Name": scalabilityOccupancyName(input.Variables.ParentID, index),
			"ParentID": input.Variables.ParentID, "Category": 6, "Size": "4096",
			"CreationTime": "1700000000000", "LastModificationTime": "1700000000000",
		})
	}
	next := ""
	if end < fixture.entries {
		next = strconv.Itoa(end)
	}
	fixture.pages.Add(1)
	fixture.children.Add(uint64(len(items)))
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"data": map[string]any{
			"getFileChildren5": map[string]any{"Items": items, "NextToken": next},
		},
	})
}

func newScalabilityOccupancyClient(t *testing.T, entries int) (*Client, *scalabilityOccupancyFixture) {
	t.Helper()
	fixture := &scalabilityOccupancyFixture{entries: entries, pageSize: 1000}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	return New(server.Client(), scalabilityOccupancyTokens{}, server.URL, "generated-key"), fixture
}

type scalabilityOccupancyEvidence struct {
	Scenario                 string `json:"scenario"`
	ConfiguredBound          int    `json:"configured_bound"`
	RetainedEntries          int    `json:"retained_entries"`
	RetainedOrderEntries     int    `json:"retained_order_entries,omitempty"`
	ParentsRequested         int    `json:"parents_requested,omitempty"`
	WarmHits                 uint64 `json:"warm_hits"`
	WarmRequests             uint64 `json:"warm_requests"`
	EvictedEntryReFetched    bool   `json:"evicted_entry_refetched"`
	FixtureChildrenGenerated int    `json:"fixture_children_generated,omitempty"`
	OldestHintRetained       bool   `json:"oldest_hint_retained"`
	NewestHintRetained       bool   `json:"newest_hint_retained"`
}

func TestScalabilityMetadataOccupancyBounds(t *testing.T) {
	t.Run("parent_snapshots", func(t *testing.T) {
		client, fixture := newScalabilityOccupancyClient(t, 0)
		ctx := context.Background()
		parentsRequested := metadataCacheMaxEntries + 8
		for index := 0; index < parentsRequested; index++ {
			if _, err := client.ListDir(ctx, fmt.Sprintf("generated-parent-%03d", index)); err != nil {
				t.Fatalf("list parent %d: %v", index, err)
			}
		}
		client.metadataMu.Lock()
		retained := len(client.metadataCache)
		client.metadataMu.Unlock()
		if retained != metadataCacheMaxEntries {
			t.Fatalf("retained snapshots = %d, want configured bound %d", retained, metadataCacheMaxEntries)
		}

		beforeWarm := client.RequestStats()
		for index := parentsRequested - metadataCacheMaxEntries; index < parentsRequested; index++ {
			if _, err := client.ListDir(ctx, fmt.Sprintf("generated-parent-%03d", index)); err != nil {
				t.Fatalf("warm parent %d: %v", index, err)
			}
		}
		afterWarm := client.RequestStats()
		warmHits := afterWarm.MetadataCacheHits - beforeWarm.MetadataCacheHits
		warmRequests := afterWarm.HTTPRequests - beforeWarm.HTTPRequests
		if warmHits != uint64(metadataCacheMaxEntries) || warmRequests != 0 {
			t.Fatalf("newest snapshot counters: hits=%d requests=%d, want %d hits and zero requests", warmHits, warmRequests, metadataCacheMaxEntries)
		}
		beforeEvicted := client.RequestStats()
		if _, err := client.ListDir(ctx, "generated-parent-000"); err != nil {
			t.Fatalf("reload oldest parent: %v", err)
		}
		afterEvicted := client.RequestStats()
		evictedReFetched := afterEvicted.MetadataCacheMisses-beforeEvicted.MetadataCacheMisses == 1 &&
			afterEvicted.HTTPRequests-beforeEvicted.HTTPRequests == 1
		if !evictedReFetched {
			t.Fatal("oldest snapshot was not evicted and re-fetched")
		}
		client.metadataMu.Lock()
		retained = len(client.metadataCache)
		client.metadataMu.Unlock()
		if retained > metadataCacheMaxEntries {
			t.Fatalf("retained snapshots after reload = %d, exceeds bound %d", retained, metadataCacheMaxEntries)
		}
		logScalabilityOccupancy(t, scalabilityOccupancyEvidence{
			Scenario: "parent_snapshots", ConfiguredBound: metadataCacheMaxEntries,
			RetainedEntries: retained, ParentsRequested: parentsRequested,
			WarmHits: warmHits, WarmRequests: warmRequests, EvictedEntryReFetched: evictedReFetched,
		})
		if fixture.pages.Load() != uint64(parentsRequested+1) {
			t.Fatalf("generated pages = %d, want %d", fixture.pages.Load(), parentsRequested+1)
		}
	})

	t.Run("item_parent_hints", func(t *testing.T) {
		generated := itemParentCacheMaxEntries + 904
		client, fixture := newScalabilityOccupancyClient(t, generated)
		items, err := client.ListDir(context.Background(), "generated-parent")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != generated {
			t.Fatalf("listing entries = %d, want %d", len(items), generated)
		}

		client.metadataMu.Lock()
		retained := len(client.itemParents)
		retainedOrder := client.itemParentOrder.Len()
		_, oldestExists := client.itemParents[scalabilityOccupancyID("generated-parent", 0)]
		_, newestExists := client.itemParents[scalabilityOccupancyID("generated-parent", generated-1)]
		client.metadataMu.Unlock()
		if retained != itemParentCacheMaxEntries || retainedOrder != itemParentCacheMaxEntries {
			t.Fatalf("hint map/order occupancy = %d/%d, want configured bound %d", retained, retainedOrder, itemParentCacheMaxEntries)
		}
		if oldestExists || !newestExists {
			t.Fatalf("hint eviction state: oldest retained=%v newest retained=%v", oldestExists, newestExists)
		}
		if fixture.pages.Load() != uint64((generated+fixture.pageSize-1)/fixture.pageSize) {
			t.Fatalf("generated pages = %d, want %d", fixture.pages.Load(), (generated+fixture.pageSize-1)/fixture.pageSize)
		}
		logScalabilityOccupancy(t, scalabilityOccupancyEvidence{
			Scenario: "item_parent_hints", ConfiguredBound: itemParentCacheMaxEntries,
			RetainedEntries: retained, RetainedOrderEntries: retainedOrder,
			FixtureChildrenGenerated: generated, OldestHintRetained: oldestExists, NewestHintRetained: newestExists,
		})
	})
}

func logScalabilityOccupancy(t *testing.T, evidence scalabilityOccupancyEvidence) {
	t.Helper()
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
}

type scalabilityPopulatedSnapshotEvidence struct {
	RetainedSnapshotAccountedBytes int64  `json:"retained_snapshot_accounted_bytes"`
	ConfiguredSnapshotByteBound    int64  `json:"configured_snapshot_byte_bound"`
	ConfiguredSnapshotItemBound    int    `json:"configured_snapshot_item_bound"`
	RetainedParentHintStringBytes  int64  `json:"retained_parent_hint_string_bytes"`
	EntriesPerParent               int    `json:"entries_per_parent"`
	ParentsRequested               int    `json:"parents_requested"`
	ConfiguredSnapshotBound        int    `json:"configured_snapshot_bound"`
	RetainedSnapshotParents        int    `json:"retained_snapshot_parents"`
	RetainedSnapshotItems          int    `json:"retained_snapshot_items"`
	SnapshotItemArrayBytes         uint64 `json:"snapshot_item_array_logical_bytes"`
	SnapshotStringBytes            uint64 `json:"snapshot_string_field_logical_bytes"`
	RetainedSnapshotPayloadBytes   uint64 `json:"retained_snapshot_logical_payload_bytes"`
	RetainedItemParentHints        int    `json:"retained_item_parent_hints"`
	RetainedHintOrderEntries       int    `json:"retained_hint_order_entries"`
	FixtureChildrenGenerated       uint64 `json:"fixture_children_generated"`
	FixturePages                   uint64 `json:"fixture_pages"`
	InitialRequests                uint64 `json:"initial_requests"`
	InitialCacheMisses             uint64 `json:"initial_cache_misses"`
	WarmCacheHits                  uint64 `json:"warm_cache_hits"`
	WarmRequests                   uint64 `json:"warm_requests"`
	EvictedParents                 int    `json:"evicted_parents"`
	EvictedParentRefetched         bool   `json:"evicted_parent_refetched"`
	ReloadRequests                 uint64 `json:"reload_requests"`
	ReloadPages                    uint64 `json:"reload_pages"`
	HeapAllocBaselineAfterGC       uint64 `json:"heap_alloc_baseline_after_gc_bytes"`
	HeapAllocAfterGC               uint64 `json:"heap_alloc_after_gc_bytes"`
	RetainedHeapDelta              int64  `json:"retained_heap_delta_bytes"`
	CumulativeAllocDelta           uint64 `json:"cumulative_alloc_bytes"`
	RetainedHeapScope              string `json:"retained_heap_scope"`
}

func checkScalabilitySnapshot(t *testing.T, client *Client, parent string, entries int) {
	t.Helper()
	items, err := client.ListDir(context.Background(), parent)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", parent, err)
	}
	if len(items) != entries {
		t.Fatalf("ListDir(%q) returned %d entries, want %d", parent, len(items), entries)
	}
	if entries > 0 && (items[0].ID != scalabilityOccupancyID(parent, 0) || items[0].Name != scalabilityOccupancyName(parent, 0) || items[0].ParentID != parent || items[entries-1].ID != scalabilityOccupancyID(parent, entries-1) || items[entries-1].ParentID != parent) {
		t.Fatalf("ListDir(%q) returned unexpected boundary metadata", parent)
	}
}

func TestScalabilityPopulatedMetadataSnapshots(t *testing.T) {
	cases := []struct {
		entries   int
		parents   int
		optIn100k bool
	}{
		{entries: 1000, parents: metadataCacheMaxEntries + 1},
		{entries: 10000, parents: 3},
		{entries: 100000, parents: 2, optIn100k: true},
	}
	for _, testCase := range cases {
		t.Run(strconv.Itoa(testCase.entries), func(t *testing.T) {
			if testCase.optIn100k && os.Getenv("RCLONE_DEGOO_SCALE_TESTS") != "1" {
				t.Skip("set RCLONE_DEGOO_SCALE_TESTS=1 for the 100,000-entry populated-snapshot measurement")
			}

			client, fixture := newScalabilityOccupancyClient(t, testCase.entries)
			runtime.GC()
			var baseline runtime.MemStats
			runtime.ReadMemStats(&baseline)
			before := client.RequestStats()
			beforePages := fixture.pages.Load()
			beforeChildren := fixture.children.Load()

			for index := 0; index < testCase.parents; index++ {
				parent := fmt.Sprintf("generated-parent-%03d", index)
				checkScalabilitySnapshot(t, client, parent, testCase.entries)
			}
			afterInitial := client.RequestStats()
			pagesPerSnapshot := uint64((testCase.entries + fixture.pageSize - 1) / fixture.pageSize)
			wantInitialPages := uint64(testCase.parents) * pagesPerSnapshot
			if got := afterInitial.HTTPRequests - before.HTTPRequests; got != wantInitialPages {
				t.Fatalf("initial requests = %d, want %d", got, wantInitialPages)
			}
			if got := afterInitial.MetadataCacheMisses - before.MetadataCacheMisses; got != uint64(testCase.parents) {
				t.Fatalf("initial cache misses = %d, want %d", got, testCase.parents)
			}
			if got := afterInitial.MetadataPages - before.MetadataPages; got != wantInitialPages || fixture.pages.Load()-beforePages != wantInitialPages {
				t.Fatalf("initial metadata/fixture pages = %d/%d, want %d", afterInitial.MetadataPages-before.MetadataPages, fixture.pages.Load()-beforePages, wantInitialPages)
			}
			if got := fixture.children.Load() - beforeChildren; got != uint64(testCase.entries*testCase.parents) {
				t.Fatalf("generated fixture children = %d, want %d", got, testCase.entries*testCase.parents)
			}

			client.metadataMu.Lock()
			retainedParents := len(client.metadataCache)
			retainedItems := 0
			retainedIDs := make([]string, 0, retainedParents)
			for parent, snapshot := range client.metadataCache {
				retainedIDs = append(retainedIDs, parent)
				if len(snapshot.items) != testCase.entries {
					client.metadataMu.Unlock()
					t.Fatalf("snapshot %q contains %d entries, want %d", parent, len(snapshot.items), testCase.entries)
				}
				retainedItems += len(snapshot.items)
			}
			retainedHints := len(client.itemParents)
			retainedHintOrder := client.itemParentOrder.Len()
			client.metadataMu.Unlock()
			wantRetainedParents := min(testCase.parents, metadataCacheMaxEntries)
			if retainedParents != wantRetainedParents || retainedItems != wantRetainedParents*testCase.entries {
				t.Fatalf("populated snapshot occupancy = %d parents/%d items, want %d/%d", retainedParents, retainedItems, wantRetainedParents, wantRetainedParents*testCase.entries)
			}
			if retainedHints != min(testCase.entries*testCase.parents, itemParentCacheMaxEntries) || retainedHintOrder != retainedHints {
				t.Fatalf("parent hint occupancy = %d/%d, want %d entries and order nodes", retainedHints, retainedHintOrder, min(testCase.entries*testCase.parents, itemParentCacheMaxEntries))
			}

			beforeWarm := client.RequestStats()
			for _, parent := range retainedIDs {
				checkScalabilitySnapshot(t, client, parent, testCase.entries)
			}
			afterWarm := client.RequestStats()
			warmHits := afterWarm.MetadataCacheHits - beforeWarm.MetadataCacheHits
			warmRequests := afterWarm.HTTPRequests - beforeWarm.HTTPRequests
			if warmHits != uint64(retainedParents) || warmRequests != 0 || afterWarm.MetadataPages != beforeWarm.MetadataPages {
				t.Fatalf("warm populated snapshots: hits=%d requests=%d pages=%d, want %d hits and no remote work", warmHits, warmRequests, afterWarm.MetadataPages-beforeWarm.MetadataPages, retainedParents)
			}

			evictedParents := testCase.parents - retainedParents
			refetched := false
			var reloadRequests, reloadPages uint64
			if evictedParents > 0 {
				retainedSet := make(map[string]struct{}, len(retainedIDs))
				for _, parent := range retainedIDs {
					retainedSet[parent] = struct{}{}
				}
				evictedParent := ""
				for index := 0; index < testCase.parents; index++ {
					candidate := fmt.Sprintf("generated-parent-%03d", index)
					if _, ok := retainedSet[candidate]; !ok {
						evictedParent = candidate
						break
					}
				}
				if evictedParent == "" {
					t.Fatal("cache was over capacity but no requested parent was evicted")
				}
				beforeReload := client.RequestStats()
				beforeReloadPages := fixture.pages.Load()
				checkScalabilitySnapshot(t, client, evictedParent, testCase.entries)
				afterReload := client.RequestStats()
				reloadRequests = afterReload.HTTPRequests - beforeReload.HTTPRequests
				reloadPages = afterReload.MetadataPages - beforeReload.MetadataPages
				if afterReload.MetadataCacheMisses-beforeReload.MetadataCacheMisses != 1 || reloadRequests != pagesPerSnapshot || reloadPages != pagesPerSnapshot || fixture.pages.Load()-beforeReloadPages != pagesPerSnapshot {
					t.Fatalf("evicted snapshot reload: misses=%d requests=%d pages=%d fixture_pages=%d, want one miss and %d requests/pages", afterReload.MetadataCacheMisses-beforeReload.MetadataCacheMisses, reloadRequests, reloadPages, fixture.pages.Load()-beforeReloadPages, pagesPerSnapshot)
				}
				checkScalabilitySnapshot(t, client, evictedParent, testCase.entries)
				afterReloadWarm := client.RequestStats()
				if afterReloadWarm.HTTPRequests != afterReload.HTTPRequests || afterReloadWarm.MetadataCacheHits-afterReload.MetadataCacheHits != 1 {
					t.Fatal("reloaded populated snapshot did not produce a warm cache hit")
				}
				refetched = true
			}

			runtime.GC()
			var afterGC runtime.MemStats
			runtime.ReadMemStats(&afterGC)
			runtime.KeepAlive(client)
			runtime.KeepAlive(fixture)
			client.metadataMu.Lock()
			finalParents := len(client.metadataCache)
			finalItems := 0
			var snapshotItemArrayBytes, snapshotStringBytes uint64
			for _, snapshot := range client.metadataCache {
				finalItems += len(snapshot.items)
				snapshotItemArrayBytes += uint64(cap(snapshot.items)) * uint64(unsafe.Sizeof(Item{}))
				for _, item := range snapshot.items {
					snapshotStringBytes += uint64(len(item.ID) + len(item.Name) + len(item.ParentID) + len(item.URL))
				}
			}
			accountedBytes, accountedItems := client.metadataBytes, client.metadataItems
			hintBytes := client.itemParentBytes
			client.metadataMu.Unlock()
			if accountedBytes > metadataCacheMaxBytes || accountedItems > metadataCacheMaxItems || accountedItems != finalItems || hintBytes > itemParentCacheMaxBytes {
				t.Fatalf("retained metadata exceeds admission bounds: bytes=%d items=%d hint_bytes=%d", accountedBytes, accountedItems, hintBytes)
			}
			if finalParents != wantRetainedParents || finalItems != wantRetainedParents*testCase.entries {
				t.Fatalf("final populated snapshot occupancy = %d parents/%d items, want %d/%d", finalParents, finalItems, wantRetainedParents, wantRetainedParents*testCase.entries)
			}

			evidence := scalabilityPopulatedSnapshotEvidence{
				RetainedSnapshotAccountedBytes: accountedBytes,
				ConfiguredSnapshotByteBound:    metadataCacheMaxBytes,
				ConfiguredSnapshotItemBound:    metadataCacheMaxItems,
				RetainedParentHintStringBytes:  hintBytes,
				EntriesPerParent:               testCase.entries, ParentsRequested: testCase.parents,
				ConfiguredSnapshotBound: metadataCacheMaxEntries,
				RetainedSnapshotParents: finalParents, RetainedSnapshotItems: finalItems,
				SnapshotItemArrayBytes:       snapshotItemArrayBytes,
				SnapshotStringBytes:          snapshotStringBytes,
				RetainedSnapshotPayloadBytes: snapshotItemArrayBytes + snapshotStringBytes,
				RetainedItemParentHints:      retainedHints, RetainedHintOrderEntries: retainedHintOrder,
				FixtureChildrenGenerated: fixture.children.Load() - beforeChildren,
				FixturePages:             fixture.pages.Load() - beforePages,
				InitialRequests:          afterInitial.HTTPRequests - before.HTTPRequests,
				InitialCacheMisses:       afterInitial.MetadataCacheMisses - before.MetadataCacheMisses,
				WarmCacheHits:            warmHits, WarmRequests: warmRequests,
				EvictedParents: evictedParents, EvictedParentRefetched: refetched,
				ReloadRequests: reloadRequests, ReloadPages: reloadPages,
				HeapAllocBaselineAfterGC: baseline.HeapAlloc, HeapAllocAfterGC: afterGC.HeapAlloc,
				RetainedHeapDelta:    int64(afterGC.HeapAlloc) - int64(baseline.HeapAlloc),
				CumulativeAllocDelta: afterGC.TotalAlloc - baseline.TotalAlloc,
				RetainedHeapScope:    "whole API test-process heap delta after GC; includes cached snapshots, bounded parent hints, httptest server/transport, and test bookkeeping; not a cache-only size. Logical snapshot payload is separately computed as Item slice capacity times sizeof(Item), plus ID/Name/ParentID/URL string lengths; it is not exact allocated bytes and excludes size-class rounding, map overhead, and possible shared string backing.",
			}
			encoded, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			t.Log(string(encoded))
		})
	}
}
