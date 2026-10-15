package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
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
			"ID": fmt.Sprintf("memory-id-%08d", index), "Name": fmt.Sprintf("memory-name-%08d", index),
			"ParentID": input.Variables.ParentID, "Category": 6, "Size": "4096",
			"CreationTime": "1700000000000", "LastModificationTime": "1700000000000",
		})
	}
	next := ""
	if end < fixture.entries {
		next = strconv.Itoa(end)
	}
	fixture.pages.Add(1)
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
		_, oldestExists := client.itemParents["memory-id-00000000"]
		_, newestExists := client.itemParents[fmt.Sprintf("memory-id-%08d", generated-1)]
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
