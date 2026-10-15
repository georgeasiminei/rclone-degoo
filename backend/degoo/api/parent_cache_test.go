package api

import (
	"fmt"
	"testing"
)

func TestParentHintsEvictionAndRefresh(t *testing.T) {
	c := &Client{}
	for i := range itemParentCacheMaxEntries {
		c.rememberItemParent(Item{ID: fmt.Sprint(i), ParentID: "original"})
	}
	// Remembering an existing ID updates both its parent and eviction recency.
	c.rememberItemParent(Item{ID: "0", ParentID: "moved"})
	c.rememberItemParent(Item{ID: "next", ParentID: "new"})
	if _, known := c.parentsForItems([]string{"1"}); known {
		t.Fatal("oldest hint survived eviction")
	}
	if parents, known := c.parentsForItems([]string{"0"}); !known || len(parents) != 1 || parents[0] != "moved" {
		t.Fatalf("refreshed hint = %v, %v", parents, known)
	}
	if len(c.itemParents) != itemParentCacheMaxEntries {
		t.Fatalf("hint count = %d", len(c.itemParents))
	}
	if c.itemParentOrder.Len() != len(c.itemParents) {
		t.Fatal("eviction order and hints differ in size")
	}
	c.forgetItemParents([]string{"0", "missing"})
	if _, known := c.parentsForItems([]string{"0"}); known {
		t.Fatal("forgotten hint survived")
	}
	c.rememberItemParent(Item{ID: "0", ParentID: "recreated"})
	for i := range itemParentCacheMaxEntries {
		c.rememberItemParent(Item{ID: fmt.Sprintf("replacement-%d", i), ParentID: "replacement"})
	}
	if _, known := c.parentsForItems([]string{"0"}); known {
		t.Fatal("recreated oldest hint survived full replacement")
	}
	if c.itemParentOrder.Len() != len(c.itemParents) {
		t.Fatal("forget/recreate retained stale eviction entries")
	}
	c.ClearMetadataCache()
	if len(c.itemParents) != 0 || c.itemParentOrder.Len() != 0 {
		t.Fatal("clear retained hints")
	}
	c.rememberItemParent(Item{ID: "after-clear", ParentID: "fresh"})
	if parents, known := c.parentsForItems([]string{"after-clear"}); !known || len(parents) != 1 || parents[0] != "fresh" {
		t.Fatalf("hint after clear = %v, %v", parents, known)
	}
}

func TestParentHintsRejectStaleMutationEpoch(t *testing.T) {
	c := &Client{}
	epoch := c.mutationEpochSnapshot()
	c.rememberItemParentAtMutationEpoch(Item{ID: "generated", ParentID: "original"}, epoch)
	c.advanceMutationEpoch()
	c.rememberItemParent(Item{ID: "generated", ParentID: "moved"})
	c.rememberItemsAtMutationEpoch([]Item{{ID: "generated", ParentID: "original"}, {ID: "stale", ParentID: "original"}}, epoch)
	if parents, known := c.parentsForItems([]string{"generated"}); !known || len(parents) != 1 || parents[0] != "moved" {
		t.Fatalf("stale response replaced moved parent: %v, %v", parents, known)
	}
	c.rememberItemParent(Item{ID: "", ParentID: "invalid"})
	c.rememberItemParent(Item{ID: "invalid", ParentID: ""})
	if len(c.itemParents) != 1 || c.itemParentOrder.Len() != 1 {
		t.Fatal("invalid or stale hints entered the cache")
	}
}

func BenchmarkParentHintInsertion(b *testing.B) {
	for _, capacity := range []int{64, itemParentCacheMaxEntries} {
		b.Run(fmt.Sprint(capacity), func(b *testing.B) {
			c := &Client{}
			ids := make([]string, capacity+1)
			for i := range ids {
				ids[i] = fmt.Sprintf("generated-%d", i)
			}
			for _, id := range ids[:capacity] {
				c.rememberItemParent(Item{ID: id, ParentID: "generated-parent"})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c.rememberItemParent(Item{ID: ids[i%len(ids)], ParentID: "generated-parent"})
			}
		})
	}
}
