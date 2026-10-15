package degoo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/lib/dircache"
)

func TestPlaceholderHintsBoundedWithoutForgetting(t *testing.T) {
	var f Fs
	f.rememberPlaceholder("")
	for i := 0; i < placeholderMaxEntries; i++ {
		f.rememberPlaceholder(fmt.Sprintf("provisional-%d", i))
	}
	if f.placeholderSaturated || len(f.placeholders) != placeholderMaxEntries {
		t.Fatal("premature saturation")
	}
	before := f.placeholderBytes
	f.rememberPlaceholder("provisional-0")
	if f.placeholderBytes != before {
		t.Fatal("duplicate counted twice")
	}
	f.rememberPlaceholder("overflow")
	if !f.placeholderSaturated || len(f.placeholders) != 0 || f.placeholderBytes != 0 {
		t.Fatal("saturation must release exact hints")
	}
	for _, id := range []string{"provisional-0", "overflow", "unseen"} {
		if !f.isPlaceholder(id) {
			t.Fatalf("saturation trusted %q", id)
		}
	}
	f.rememberPlaceholder("later")
	if len(f.placeholders) != 0 {
		t.Fatal("saturated tracker grew")
	}
}

func TestPlaceholderStringBudget(t *testing.T) {
	var f Fs
	f.rememberPlaceholder(strings.Repeat("a", placeholderMaxBytes))
	if f.placeholderBytes != placeholderMaxBytes || f.placeholderSaturated {
		t.Fatal("exact budget not admitted")
	}
	f.rememberPlaceholder("b")
	if !f.placeholderSaturated || !f.isPlaceholder("b") {
		t.Fatal("byte overflow forgot a provisional ID")
	}
	var oversized Fs
	oversized.rememberPlaceholder(strings.Repeat("x", placeholderMaxBytes+1))
	if !oversized.placeholderSaturated || oversized.placeholderBytes != 0 {
		t.Fatal("oversized hint retained")
	}
}

func TestPlaceholderHintsConcurrent(t *testing.T) {
	var f Fs
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 1024; i++ {
				id := fmt.Sprintf("%d-%d", worker, i)
				f.rememberPlaceholder(id)
				if !f.isPlaceholder(id) {
					t.Errorf("forgot observed ID")
				}
			}
		}(worker)
	}
	wg.Wait()
	if !f.placeholderSaturated {
		t.Fatal("expected saturation")
	}
}

func TestFindDirRevalidatesProvisionalAncestor(t *testing.T) {
	mock := newMockDegoo()
	mock.items["promoted"] = mockItem{id: "promoted", name: "folder", parentID: "vault", category: 2}
	mock.items["new-child"] = mockItem{id: "new-child", name: "child", parentID: "promoted", category: 2}
	f, server := newTestFs(t, mock)
	defer server.Close()
	f.dirCache.Put("folder", "old-placeholder")
	f.dirCache.Put("folder/child", "old-child")
	f.rememberPlaceholder("old-placeholder")
	id, err := f.findDir(context.Background(), "folder/child", false)
	if err != nil || id != "new-child" {
		t.Fatalf("lookup=%q,%v; want promoted subtree", id, err)
	}
}

func TestSaturatedPlaceholderHintsRevalidateAfterFlush(t *testing.T) {
	mock := newMockDegoo()
	mock.items["current"] = mockItem{id: "current", name: "folder", parentID: "vault", category: 2}
	f, server := newTestFs(t, mock)
	defer server.Close()
	f.rememberPlaceholder(strings.Repeat("x", placeholderMaxBytes+1))
	f.DirCacheFlush()
	f.dirCache.Put("folder", "old")
	id, err := f.findDir(context.Background(), "folder", false)
	if err != nil || id != "current" {
		t.Fatalf("lookup=%q,%v; saturation lost across cache flush", id, err)
	}
}

func TestDirectoryCacheBoundsAfterWideListing(t *testing.T) {
	mock := newMockDegoo()
	const directories = 10005
	for i := 0; i < directories; i++ {
		id := fmt.Sprintf("dir-%05d", i)
		mock.items[id] = mockItem{id: id, name: id, parentID: "vault", category: 2}
	}
	f, server := newTestFs(t, mock)
	defer server.Close()
	entries, err := f.List(context.Background(), "")
	if err != nil || len(entries) != directories {
		t.Fatalf("complete listing entries=%d err=%v", len(entries), err)
	}
	retained := 1 // pinned root
	stringBytes := 0
	for i := 0; i < directories; i++ {
		name := fmt.Sprintf("dir-%05d", i)
		if id, ok := f.dirCache.Get(name); ok {
			retained++
			stringBytes += len(name) + len(id)
		}
	}
	if retained > directoryCacheMaxEntries || retained < 2 || stringBytes > directoryCacheMaxStringBytes {
		t.Fatalf("retained=%d strings=%d", retained, stringBytes)
	}
	for _, remote := range []string{"dir-00000", "dir-10004"} {
		id, err := f.findDir(context.Background(), remote, false)
		if err != nil || id != remote {
			t.Fatalf("find evicted/retained %q = %q,%v", remote, id, err)
		}
	}
}

func TestDirMoveRevalidatesProvisionalAncestor(t *testing.T) {
	mock := newMockDegoo()
	mock.items["promoted"] = mockItem{id: "promoted", name: "folder", parentID: "vault", category: 2}
	mock.items["new-child"] = mockItem{id: "new-child", name: "child", parentID: "promoted", category: 2}
	f, server := newTestFs(t, mock)
	defer server.Close()
	f.dirCache.Put("folder", "old-placeholder")
	f.dirCache.Put("folder/child", "old-child")
	f.rememberPlaceholder("old-placeholder")
	if err := f.DirMove(context.Background(), f, "folder/child", "moved"); err != nil {
		t.Fatal(err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	item := mock.items["new-child"]
	if item.parentID != "vault" || item.name != "moved" {
		t.Fatalf("moved item=%#v", item)
	}
}

type directoryResolutionFixture struct {
	find func(parentID, leaf string) (string, bool, error)
}

func (fixture directoryResolutionFixture) FindLeaf(_ context.Context, parentID, leaf string) (string, bool, error) {
	return fixture.find(parentID, leaf)
}

func (fixture directoryResolutionFixture) CreateDir(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("unexpected create")
}

func TestFindDirRetriesHintLearnedDuringResolution(t *testing.T) {
	var f Fs
	fixture := directoryResolutionFixture{find: func(parentID, leaf string) (string, bool, error) {
		if parentID == "old-folder" && leaf == "child" {
			f.rememberPlaceholder("old-folder")
			return "old-child", true, nil
		}
		if parentID == "vault" && leaf == "folder" {
			return "new-folder", true, nil
		}
		if parentID == "new-folder" && leaf == "child" {
			return "new-child", true, nil
		}
		return "", false, nil
	}}
	f.dirCache = dircache.NewWithLimits("", "vault", fixture, dircache.Limits{MaxEntries: 16, Admit: func(_, id string) bool { return !f.isPlaceholder(id) }})
	if err := f.dirCache.FindRoot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	f.dirCache.Put("folder", "old-folder")
	id, err := f.findDir(context.Background(), "folder/child", false)
	if err != nil || id != "new-child" {
		t.Fatalf("lookup=%q,%v; want revalidated child", id, err)
	}
}

func TestDirectoryCacheRejectsProvisionalChains(t *testing.T) {
	var f Fs
	f.dirCache = newDirectoryCache("", "vault", &f)
	f.rememberPlaceholder("provisional")
	f.dirCache.Put("folder", "provisional")
	f.dirCache.Put("folder/child", "child")
	for _, remote := range []string{"folder", "folder/child"} {
		if _, ok := f.dirCache.Get(remote); ok {
			t.Fatalf("cached provisional chain at %q", remote)
		}
	}
	f.rememberPlaceholder(strings.Repeat("x", placeholderMaxBytes+1))
	f.dirCache.Put("other", "otherwise-stable")
	if _, ok := f.dirCache.Get("other"); ok {
		t.Fatal("saturated tracker admitted new mapping")
	}
}

func TestDirectoryResolutionHintChangesHaveRetryBound(t *testing.T) {
	var f Fs
	calls := 0
	fixture := directoryResolutionFixture{find: func(parentID, leaf string) (string, bool, error) {
		calls++
		id := fmt.Sprintf("provisional-%d", calls)
		f.rememberPlaceholder(id)
		return id, true, nil
	}}
	f.dirCache = dircache.NewWithLimits("", "vault", fixture, dircache.Limits{MaxEntries: 16, Admit: func(_, id string) bool { return !f.isPlaceholder(id) }})
	if err := f.dirCache.FindRoot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	id, err := f.findDir(context.Background(), "folder", false)
	if err == nil || id != "" || calls != 3 || !strings.Contains(err.Error(), "changed during resolution") {
		t.Fatalf("lookup=%q,%v calls=%d; want bounded refusal", id, err, calls)
	}
}

func TestSaturatedDirectoryResolutionDoesNotLoop(t *testing.T) {
	var f Fs
	f.rememberPlaceholder(strings.Repeat("x", placeholderMaxBytes+1))
	calls := 0
	fixture := directoryResolutionFixture{find: func(parentID, leaf string) (string, bool, error) { calls++; return "stable", true, nil }}
	f.dirCache = dircache.NewWithLimits("", "vault", fixture, dircache.Limits{MaxEntries: 16, Admit: func(_, id string) bool { return !f.isPlaceholder(id) }})
	if err := f.dirCache.FindRoot(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	id, err := f.findDir(context.Background(), "folder", false)
	if err != nil || id != "stable" || calls != 1 {
		t.Fatalf("lookup=%q,%v calls=%d", id, err, calls)
	}
}

func TestDirectoryHintChangeDoesNotHideProviderFault(t *testing.T) {
	for _, fault := range []error{&api.RateLimitError{Until: time.Now().Add(time.Hour)}, context.DeadlineExceeded, errors.New("authentication unavailable")} {
		t.Run(fmt.Sprintf("%T-%s", fault, fault.Error()), func(t *testing.T) {
			var f Fs
			calls := 0
			fixture := directoryResolutionFixture{find: func(parentID, leaf string) (string, bool, error) {
				calls++
				f.rememberPlaceholder("unrelated")
				return "", false, fault
			}}
			f.dirCache = dircache.NewWithLimits("", "vault", fixture, dircache.Limits{MaxEntries: 16})
			if err := f.dirCache.FindRoot(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			if _, err := f.findDir(context.Background(), "folder", false); !errors.Is(err, fault) || calls != 1 {
				t.Fatalf("fault=%v calls=%d; want original fault without retry", err, calls)
			}
		})
	}
}

func TestNestedConfiguredRootRevalidatesBeforeChildLookup(t *testing.T) {
	mock := newMockDegoo()
	mock.items["old-root"] = mockItem{
		id: "old-root", name: "folder", parentID: "vault", category: 6,
	}
	mock.items["old-root-marker"] = mockItem{
		id: "old-root-marker", name: directoryMarkerName("old-root"), parentID: "old-root", category: 6,
	}
	f, server, err := newTestFsAtRoot(t, mock, "folder")
	defer server.Close()
	if err != nil {
		t.Fatalf("NewFs with nested root: %v", err)
	}
	deg, ok := f.(*Fs)
	if !ok {
		t.Fatalf("NewFs returned %T, want *Fs", f)
	}
	if got, err := deg.dirCache.RootID(context.Background(), false); err != nil || got != "old-root" {
		t.Fatalf("initial configured root ID = %q, %v; want old-root", got, err)
	}

	// Simulate provider promotion while the cache still pins the old Cat6 ID.
	mock.mu.Lock()
	delete(mock.items, "old-root-marker")
	mock.items["promoted-root"] = mockItem{
		id: "promoted-root", name: "folder", parentID: "vault", category: 2,
	}
	mock.items["promoted-root-marker"] = mockItem{
		id: "promoted-root-marker", name: directoryMarkerName("old-root"), parentID: "promoted-root", category: 6,
	}
	mock.items["promoted-child"] = mockItem{
		id: "promoted-child", name: "child", parentID: "promoted-root", category: 2,
	}
	mock.mu.Unlock()

	id, err := deg.findDir(context.Background(), "child", false)
	if err != nil || id != "promoted-child" {
		t.Fatalf("nested-root child lookup = %q, %v; want promoted-child", id, err)
	}
	if got, err := deg.dirCache.RootID(context.Background(), false); err != nil || got != "promoted-root" {
		t.Fatalf("revalidated configured root ID = %q, %v; want promoted-root", got, err)
	}
}
