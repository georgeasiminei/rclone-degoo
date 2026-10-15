package dircache

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

type testCacher struct {
	children map[string]map[string]string
}

type changingRootCacher struct {
	parentLookups int
	rootParentID  string
}

type aliasRootCacher struct {
	dc *DirCache
}

func (f *testCacher) FindLeaf(_ context.Context, parentID, leaf string) (string, bool, error) {
	id, ok := f.children[parentID][leaf]
	return id, ok, nil
}

func (f *testCacher) CreateDir(_ context.Context, parentID, leaf string) (string, error) {
	if f.children[parentID] == nil {
		f.children[parentID] = make(map[string]string)
	}
	id := parentID + "/" + leaf
	f.children[parentID][leaf] = id
	return id, nil
}

func (f *changingRootCacher) FindLeaf(_ context.Context, parentID, leaf string) (string, bool, error) {
	if parentID == "true-root" && leaf == "a" {
		f.parentLookups++
		return fmt.Sprintf("parent-%d", f.parentLookups), true, nil
	}
	if leaf == "vault" {
		f.rootParentID = parentID
		return "vault-under-" + parentID, true, nil
	}
	return "", false, nil
}

func (f *changingRootCacher) CreateDir(_ context.Context, parentID, leaf string) (string, error) {
	return "created-under-" + parentID + "/" + leaf, nil
}

func (f *aliasRootCacher) FindLeaf(_ context.Context, parentID, leaf string) (string, bool, error) {
	if parentID == "true-root" && leaf == "vault" {
		f.dc.SetRootIDAlias("aliased-root")
		return "vault-id", true, nil
	}
	return "", false, nil
}

func (f *aliasRootCacher) CreateDir(_ context.Context, parentID, leaf string) (string, error) {
	return "created-under-" + parentID + "/" + leaf, nil
}

func TestBoundedDirCacheEvictsSubtreesAndKeepsInverseConsistent(t *testing.T) {
	dc := NewWithLimits("", "root-id", nil, Limits{MaxEntries: 3})
	dc.Put("a", "a-id")
	dc.Put("a/child", "child-id")
	dc.Put("b", "b-id")

	if len(dc.cache) > 3 {
		t.Fatalf("cache has %d entries, limit is 3", len(dc.cache))
	}
	if _, ok := dc.Get("a"); ok {
		t.Fatal("oldest ancestor remained cached")
	}
	if _, ok := dc.Get("a/child"); ok {
		t.Fatal("descendant of evicted ancestor remained cached")
	}
	if _, ok := dc.GetInv("a-id"); ok {
		t.Fatal("inverse entry for evicted ancestor remained cached")
	}
	if got, ok := dc.GetInv("b-id"); !ok || got != "b" {
		t.Fatalf("GetInv(b-id) = %q, %t; want b, true", got, ok)
	}
	if got, ok := dc.GetInv("root-id"); !ok || got != "" {
		t.Fatalf("root inverse = %q, %t; want empty path, true", got, ok)
	}
}

func TestBoundedDirCacheAdmissionBytesAndReplacement(t *testing.T) {
	dc := NewWithLimits("", "root-id", nil, Limits{MaxStringBytes: 16})
	dc.Put("a", "one")
	dc.Put("a/child", "leaf")
	dc.Put("a", "two")

	if _, ok := dc.GetInv("one"); ok {
		t.Fatal("replaced ID retained an inverse entry")
	}
	if _, ok := dc.Get("a/child"); ok {
		t.Fatal("replacing a directory ID retained a stale descendant")
	}
	if got, ok := dc.GetInv("two"); !ok || got != "a" {
		t.Fatalf("GetInv(two) = %q, %t; want a, true", got, ok)
	}
	if dc.cacheBytes > 16 {
		t.Fatalf("cached string bytes = %d, limit is 16", dc.cacheBytes)
	}

	dc.Put("a-very-long-path", "id")
	if _, ok := dc.Get("a-very-long-path"); ok {
		t.Fatal("oversized path was admitted")
	}
	if dc.cacheBytes > 16 {
		t.Fatalf("cached string bytes after oversized admission = %d, limit is 16", dc.cacheBytes)
	}
}

func TestDirCacheAdmissionPredicateRejectsAndPurgesSubtree(t *testing.T) {
	dc := NewWithLimits("", "root-id", nil, Limits{
		MaxEntries: 8,
		Admit: func(_, id string) bool {
			return id != "provisional"
		},
	})
	dc.Put("a", "a-id")
	dc.Put("a/child", "child-id")
	dc.Put("a", "provisional")
	for _, path := range []string{"a", "a/child"} {
		if _, ok := dc.Get(path); ok {
			t.Fatalf("rejected path %q remained cached", path)
		}
	}
	for _, id := range []string{"a-id", "child-id", "provisional"} {
		if _, ok := dc.GetInv(id); ok {
			t.Fatalf("inverse entry for %q remained cached", id)
		}
	}
	if dc.cacheBytes != 0 || dc.cacheLRU.Len() != 0 {
		t.Fatalf("after rejection: bytes=%d LRU=%d; want zero", dc.cacheBytes, dc.cacheLRU.Len())
	}
}

func TestDirCacheAdmissionPredicateWithoutCapacityLimit(t *testing.T) {
	dc := NewWithLimits("", "root-id", nil, Limits{
		Admit: func(_, id string) bool { return id != "rejected" },
	})
	dc.Put("kept", "kept-id")
	dc.Put("rejected", "rejected")
	if got, ok := dc.Get("kept"); !ok || got != "kept-id" {
		t.Fatalf("kept lookup = %q, %t; want kept-id, true", got, ok)
	}
	if _, ok := dc.Get("rejected"); ok {
		t.Fatal("predicate-only cache admitted rejected mapping")
	}
}

func TestBoundedDirCacheFlushDirRepairsInverseAlias(t *testing.T) {
	dc := NewWithLimits("", "root-id", nil, Limits{MaxEntries: 4})
	dc.Put("a", "shared-id")
	dc.Put("b", "shared-id")
	if got, ok := dc.GetInv("shared-id"); !ok || got != "b" {
		t.Fatalf("latest inverse = %q, %t; want b, true", got, ok)
	}

	dc.FlushDir("b")
	if got, ok := dc.GetInv("shared-id"); !ok || got != "a" {
		t.Fatalf("repaired inverse = %q, %t; want a, true", got, ok)
	}
	if dc.cacheBytes != len("a")+len("shared-id") {
		t.Fatalf("cached string bytes = %d, want %d", dc.cacheBytes, len("a")+len("shared-id"))
	}
}

func TestBoundedDirCacheRootParentSurvivesEviction(t *testing.T) {
	cacher := &testCacher{children: map[string]map[string]string{
		"true-root": {"a": "a-id"},
		"a-id":      {"b": "b-id"},
	}}
	dc := NewWithLimits("a/b", "true-root", cacher, Limits{MaxEntries: 1, MaxStringBytes: 1})
	if err := dc.FindRoot(context.Background(), false); err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	if got, err := dc.RootParentID(context.Background(), false); err != nil || got != "a-id" {
		t.Fatalf("RootParentID() = %q, %v; want a-id, nil", got, err)
	}
	if got, err := dc.RootID(context.Background(), false); err != nil || got != "b-id" {
		t.Fatalf("RootID() = %q, %v; want b-id, nil", got, err)
	}
}

func TestBoundedDirCacheRootUsesCapturedParentForFinalLookup(t *testing.T) {
	cacher := &changingRootCacher{}
	dc := NewWithLimits("a/vault", "true-root", cacher, Limits{MaxEntries: 1})
	if err := dc.FindRoot(context.Background(), false); err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	parentID, err := dc.RootParentID(context.Background(), false)
	if err != nil {
		t.Fatalf("RootParentID: %v", err)
	}
	rootID, err := dc.RootID(context.Background(), false)
	if err != nil {
		t.Fatalf("RootID: %v", err)
	}
	if cacher.parentLookups != 1 {
		t.Fatalf("parent lookups = %d, want 1", cacher.parentLookups)
	}
	if parentID != cacher.rootParentID {
		t.Fatalf("RootParentID = %q, leaf lookup used %q", parentID, cacher.rootParentID)
	}
	if rootID != "vault-under-"+parentID {
		t.Fatalf("RootID = %q, want vault under captured parent %q", rootID, parentID)
	}
}

func TestBoundedDirCacheRootParentUsesUpdatedRootAlias(t *testing.T) {
	cacher := &aliasRootCacher{}
	dc := NewWithLimits("vault", "true-root", cacher, Limits{MaxEntries: 1})
	cacher.dc = dc
	if err := dc.FindRoot(context.Background(), false); err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	if got, err := dc.RootParentID(context.Background(), false); err != nil || got != "aliased-root" {
		t.Fatalf("RootParentID() = %q, %v; want aliased-root, nil", got, err)
	}
	if got, err := dc.RootID(context.Background(), false); err != nil || got != "vault-id" {
		t.Fatalf("RootID() = %q, %v; want vault-id, nil", got, err)
	}
}

func TestBoundedDirCacheGetAndGetInvRefreshLRU(t *testing.T) {
	dc := NewWithLimits("", "root-id", nil, Limits{MaxEntries: 4})
	dc.Put("a", "a-id")
	dc.Put("b", "b-id")
	dc.Put("c", "c-id")
	if _, ok := dc.Get("a"); !ok {
		t.Fatal("Get(a) missed")
	}
	if _, ok := dc.GetInv("b-id"); !ok {
		t.Fatal("GetInv(b-id) missed")
	}
	dc.Put("d", "d-id")
	for _, path := range []string{"a", "b", "d"} {
		if _, ok := dc.Get(path); !ok {
			t.Fatalf("recent path %q was evicted", path)
		}
	}
	if _, ok := dc.Get("c"); ok {
		t.Fatal("least-recently-used path c remained cached")
	}
}

func TestBoundedDirCacheFlushAndResetAccounting(t *testing.T) {
	dc := NewWithLimits("", "root-id", nil, Limits{MaxEntries: 4, MaxStringBytes: 32})
	dc.Put("a", "a-id")
	dc.Put("a/child", "child-id")
	dc.FlushDir("a")
	if dc.cacheBytes != 0 || dc.cacheLRU.Len() != 0 {
		t.Fatalf("after FlushDir: bytes=%d LRU=%d; want zero", dc.cacheBytes, dc.cacheLRU.Len())
	}
	if _, ok := dc.GetInv("a-id"); ok {
		t.Fatal("FlushDir retained inverse entry")
	}
	dc.Put("b", "b-id")
	dc.ResetRoot()
	if dc.cacheBytes != 0 || dc.cacheLRU.Len() != 0 {
		t.Fatalf("after ResetRoot: bytes=%d LRU=%d; want zero", dc.cacheBytes, dc.cacheLRU.Len())
	}
	if got, ok := dc.GetInv("root-id"); !ok || got != "" {
		t.Fatalf("root after ResetRoot = %q, %t; want empty path, true", got, ok)
	}
}

func TestBoundedDirCacheConcurrentAccess(t *testing.T) {
	dc := NewWithLimits("", "root-id", nil, Limits{MaxEntries: 32, MaxStringBytes: 256})
	const workers = 8
	const iterations = 100
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			parent := fmt.Sprintf("dir-%d", worker)
			child := parent + "/child"
			parentID := fmt.Sprintf("id-%d", worker)
			childID := fmt.Sprintf("child-id-%d", worker)
			for i := 0; i < iterations; i++ {
				dc.Put(parent, parentID)
				dc.Put(child, childID)
				dc.Get(child)
				dc.GetInv(childID)
				if i%3 == 0 {
					dc.FlushDir(parent)
				}
			}
		}(worker)
	}
	wg.Wait()
	dc.Put("stable", "stable-id")
	if got, ok := dc.Get("stable"); !ok || got != "stable-id" {
		t.Fatalf("stable lookup = %q, %t; want stable-id, true", got, ok)
	}
	if got, ok := dc.GetInv("stable-id"); !ok || got != "stable" {
		t.Fatalf("stable inverse = %q, %t; want stable, true", got, ok)
	}
}

func TestDirCacheDefaultRemainsUnbounded(t *testing.T) {
	dc := New("", "root-id", nil)
	for i := 0; i < 20; i++ {
		path := fmt.Sprintf("dir-%d", i)
		dc.Put(path, fmt.Sprintf("id-%d", i))
	}
	if got := len(dc.cache); got != 21 {
		t.Fatalf("default cache entries = %d, want 21 including root", got)
	}
}
