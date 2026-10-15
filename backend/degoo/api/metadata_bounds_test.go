package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMetadataOversizedSnapshotStillReturnsCompleteListing(t *testing.T) {
	c, fixture := newScalabilityOccupancyClient(t, 5)
	c.metadataBudget = metadataCacheBudget{bytes: 1024, items: 2}
	for range 2 {
		items, err := c.ListDir(context.Background(), "parent")
		if err != nil || len(items) != 5 || items[4].ID != scalabilityOccupancyID("parent", 4) {
			t.Fatalf("oversized directory was truncated: count=%d err=%v", len(items), err)
		}
		if len(c.metadataCache) != 0 || c.metadataBytes != 0 || c.metadataItems != 0 {
			t.Fatal("oversized directory entered the retained cache")
		}
	}
	if fixture.pages.Load() != 2 {
		t.Fatal("uncached complete listing did not refetch")
	}
	items, err := c.ListDirMatches(context.Background(), "parent", scalabilityOccupancyName("parent", 4), "")
	if err != nil || len(items) != 1 || fixture.pages.Load() != 3 {
		t.Fatalf("uncached selector lost the requested item: %+v %v", items, err)
	}
}

func TestMetadataCachedSelectorHonorsCooldownAndInvalidation(t *testing.T) {
	c, fixture := newScalabilityOccupancyClient(t, 5)
	name := scalabilityOccupancyName("parent", 4)
	if _, err := c.ListDirMatches(context.Background(), "parent", name, ""); err != nil {
		t.Fatal(err)
	}
	c.rateLimitMu.Lock()
	c.rateLimitTill = time.Now().Add(time.Minute)
	c.rateLimitMu.Unlock()
	_, err := c.ListDirMatches(context.Background(), "parent", name, "")
	var cooldown *RateLimitError
	if !errors.As(err, &cooldown) || fixture.pages.Load() != 1 {
		t.Fatalf("cached selection bypassed cooldown: %v", err)
	}
	c.rateLimitMu.Lock()
	c.rateLimitTill = time.Time{}
	c.rateLimitMu.Unlock()
	c.invalidateMetadataParents([]string{"parent"}, true)
	if _, err := c.ListDirMatches(context.Background(), "parent", name, ""); err != nil || fixture.pages.Load() != 2 {
		t.Fatalf("selector reused an invalidated index: %v", err)
	}
}

func TestMetadataSnapshotBudgetAndRelease(t *testing.T) {
	c := &Client{metadataBudget: metadataCacheBudget{bytes: 4096, items: 3}}
	put := func(parent string, items []Item) {
		c.cacheListDirLocked(parent, items, c.metadataGeneration, c.parentGenerationLocked(parent))
	}
	items := []Item{{ID: "a", Name: "first"}, {ID: "b", Name: "second"}}
	put("one", items)
	if c.metadataItems != 2 || c.metadataBytes == 0 || c.metadataBytes > 4096 {
		t.Fatalf("initial accounting: items=%d bytes=%d", c.metadataItems, c.metadataBytes)
	}
	put("two", items)
	if len(c.metadataCache) != 1 || c.metadataItems != 2 || c.metadataCache["two"].items == nil {
		t.Fatal("record admission did not evict the oldest snapshot")
	}
	before := c.metadataBytes
	put("oversized", []Item{{ID: "large", Name: strings.Repeat("x", 4096)}})
	if len(c.metadataCache) != 1 || c.metadataBytes != before {
		t.Fatal("oversized snapshot displaced valid retained metadata")
	}
	put("two", items[:1])
	if c.metadataItems != 1 || c.metadataBytes >= before {
		t.Fatal("replacement failed to release accounting")
	}
	c.invalidateMetadataParents([]string{"two"}, true)
	if c.metadataItems != 0 || c.metadataBytes != 0 {
		t.Fatal("mutation retained snapshot accounting")
	}
	put("one", items)
	c.pruneMetadataCacheLocked(time.Now().Add(metadataCacheTTL + time.Second))
	if c.metadataItems != 0 || c.metadataBytes != 0 {
		t.Fatal("expiry retained snapshot accounting")
	}
	put("one", items)
	c.ClearMetadataCache()
	if c.metadataItems != 0 || c.metadataBytes != 0 {
		t.Fatal("global clear retained snapshot accounting")
	}
}

func TestMetadataSnapshotByteAdmissionEvictsOldest(t *testing.T) {
	c := &Client{metadataBudget: metadataCacheBudget{bytes: 4096, items: 100}}
	for _, parent := range []string{"one", "two", "three", "four"} {
		c.cacheListDirLocked(parent, []Item{{ID: parent, Name: strings.Repeat("x", 900)}}, 0, 0)
		if c.metadataBytes > 4096 || c.metadataItems > 100 {
			t.Fatal("byte admission exceeded its resource budget")
		}
	}
	if _, retained := c.metadataCache["one"]; retained || len(c.metadataCache) == 0 || c.metadataCache["four"].items == nil {
		t.Fatal("byte pressure did not evict oldest metadata and retain the new snapshot")
	}
	var wantBytes int64
	var wantItems int
	for _, entry := range c.metadataCache {
		wantBytes += entry.bytes
		wantItems += len(entry.items)
	}
	if c.metadataBytes != wantBytes || c.metadataItems != wantItems {
		t.Fatal("eviction accounting differs from retained content")
	}
	c.cacheListDirLocked("four", []Item{{ID: "too-large", Name: strings.Repeat("x", 4096)}}, 0, 0)
	if _, stale := c.metadataCache["four"]; stale {
		t.Fatal("oversized replacement left its stale predecessor cached")
	}
}

func TestMetadataSnapshotNameSelectionIsolation(t *testing.T) {
	c := &Client{}
	items := []Item{
		{ID: "backup-b", Name: "backup-target-b"},
		{ID: "exact-a", Name: "target"},
		{ID: "unrelated", Name: "other"},
		{ID: "backup-a", Name: "backup-target-a"},
		{ID: "exact-b", Name: "target"},
	}
	c.cacheListDirLocked("parent", items, 0, 0)
	got, err := c.ListDirMatches(context.Background(), "parent", "target", "backup-target-")
	if err != nil || len(got) != 2 || got[0].ID != "exact-a" || got[1].ID != "exact-b" {
		t.Fatalf("exact-name selection: %+v %v", got, err)
	}
	got[0].ID = "caller-modified"
	got, err = c.ListDirMatches(context.Background(), "parent", "absent", "backup-target-")
	if err != nil || len(got) != 2 || got[0].ID != "backup-b" || got[1].ID != "backup-a" {
		t.Fatalf("prefix fallback did not preserve source order: %+v %v", got, err)
	}
	got, err = c.ListDirMatches(context.Background(), "parent", "target", "")
	if err != nil || got[0].ID != "exact-a" {
		t.Fatal("caller changed the retained snapshot")
	}
	if c.RequestStats().HTTPRequests != 0 || c.RequestStats().MetadataCacheHits != 3 {
		t.Fatal("warm selectors issued a request or lost cache accounting")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ListDirMatches(ctx, "parent", "target", ""); err == nil {
		t.Fatal("cached selector ignored cancellation")
	}
}

func TestParentHintByteBudget(t *testing.T) {
	c := &Client{}
	c.rememberItemParent(Item{ID: "small", ParentID: "parent"})
	c.rememberItemParent(Item{ID: strings.Repeat("x", itemParentCacheMaxBytes), ParentID: "parent"})
	if len(c.itemParents) != 1 || c.itemParentBytes != int64(len("smallparent")) {
		t.Fatal("oversized hint was retained or evicted useful metadata")
	}
	for index := 0; index < 20; index++ {
		id := strings.Repeat(string(rune('a'+index)), itemParentCacheMaxBytes/8)
		c.rememberItemParent(Item{ID: id, ParentID: "parent"})
		if c.itemParentBytes > itemParentCacheMaxBytes || c.itemParentOrder.Len() != len(c.itemParents) {
			t.Fatal("hint byte admission exceeded its bound")
		}
	}
	c.forgetItemParents([]string{c.itemParentOrder.Front().Value.(string)})
	var want int64
	for id, entry := range c.itemParents {
		want += int64(len(id) + len(entry.parentID))
	}
	if c.itemParentBytes != want {
		t.Fatal("forgetting a hint lost byte accounting")
	}
	c.ClearMetadataCache()
	if c.itemParentBytes != 0 {
		t.Fatal("clear retained hint byte accounting")
	}
}
