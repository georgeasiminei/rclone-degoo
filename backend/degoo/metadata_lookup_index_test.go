package degoo

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestLookupChildIndexedWarmNameResolution(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(*testing.T, *mockDegoo)
		wantID      string
		wantDir     bool
		wantGhostID string
		wantError   string
		wantPages   uint64
	}{
		{
			name: "exact item takes precedence over backup",
			setup: func(t *testing.T, mock *mockDegoo) {
				backupName, err := newBackupName("cipher")
				if err != nil {
					t.Fatal(err)
				}
				mock.items["active"] = mockItem{id: "active", name: "cipher", parentID: "vault", category: 6, size: 4}
				mock.items["backup"] = mockItem{id: "backup", name: backupName, parentID: "vault", category: 6, size: 3}
			},
			wantID:    "active",
			wantPages: 1,
		},
		{
			name: "backup-only recovery",
			setup: func(t *testing.T, mock *mockDegoo) {
				backupName, err := newBackupName("cipher")
				if err != nil {
					t.Fatal(err)
				}
				mock.items["backup"] = mockItem{id: "backup", name: backupName, parentID: "vault", category: 6, size: 3}
			},
			wantID:    "backup",
			wantPages: 1,
		},
		{
			name: "duplicate exact names remain ambiguous",
			setup: func(_ *testing.T, mock *mockDegoo) {
				mock.items["first"] = mockItem{id: "first", name: "cipher", parentID: "vault", category: 6, size: 4}
				mock.items["second"] = mockItem{id: "second", name: "cipher", parentID: "vault", category: 6, size: 5}
			},
			wantError: "ambiguous duplicate name",
			wantPages: 1,
		},
		{
			name: "promoted placeholder remains a directory",
			setup: func(_ *testing.T, mock *mockDegoo) {
				mock.items["directory"] = mockItem{id: "directory", name: "cipher", parentID: "vault", category: 2}
				mock.items["placeholder"] = mockItem{id: "placeholder", name: "cipher", parentID: "vault", category: 6}
				markerName := directoryMarkerName("placeholder")
				mock.items["marker"] = mockItem{id: "marker", name: markerName, parentID: "placeholder", category: 6}
			},
			wantID:      "directory",
			wantDir:     true,
			wantGhostID: "placeholder",
			wantPages:   2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock := newMockDegoo()
			test.setup(t, mock)
			f, server := newTestFs(t, mock)
			defer server.Close()

			before := f.RequestStats()
			for attempt := 0; attempt < 2; attempt++ {
				item, isDir, ghosts, err := f.lookupChild(context.Background(), "vault", "cipher")
				if test.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), test.wantError) {
						t.Fatalf("lookup %d error = %v, want text %q", attempt+1, err, test.wantError)
					}
					continue
				}
				if err != nil {
					t.Fatalf("lookup %d: %v", attempt+1, err)
				}
				if item == nil || item.ID != test.wantID || isDir != test.wantDir {
					t.Fatalf("lookup %d = (%+v, dir=%t), want ID %q and dir=%t", attempt+1, item, isDir, test.wantID, test.wantDir)
				}
				if test.wantGhostID == "" {
					if len(ghosts) != 0 {
						t.Fatalf("lookup %d ghosts = %+v, want none", attempt+1, ghosts)
					}
				} else if len(ghosts) != 1 || ghosts[0].ID != test.wantGhostID {
					t.Fatalf("lookup %d ghosts = %+v, want placeholder %q", attempt+1, ghosts, test.wantGhostID)
				}
			}

			after := f.RequestStats()
			if pages := after.MetadataPages - before.MetadataPages; pages != test.wantPages {
				t.Fatalf("listing pages = %d, want %d across both lookups", pages, test.wantPages)
			}
			if requests := after.RequestsByOperation["GetFileChildren5"] - before.RequestsByOperation["GetFileChildren5"]; requests != test.wantPages {
				t.Fatalf("listing requests = %d, want %d for both lookups", requests, test.wantPages)
			}
		})
	}
}

func TestLookupChildIndexedWarmAllocationBounds(t *testing.T) {
	const iterations = 16
	var smallBytes uint64
	var largestBytes uint64

	for _, count := range []int{1000, 10000, 100000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			if count == 100000 && os.Getenv("RCLONE_DEGOO_SCALE_TESTS") != "1" {
				t.Skip("set RCLONE_DEGOO_SCALE_TESTS=1 for the 100,000-entry lookup measurement")
			}
			f, fixture := newScalabilityFs(t, count, 1000)
			name := fmt.Sprintf("object-%08d", count-1)
			lookup := func() {
				item, isDir, _, err := f.lookupChild(context.Background(), "vault", name)
				if err != nil || item == nil || item.Name != name || isDir {
					t.Fatalf("lookup = (%+v, dir=%t, err=%v), want file %q", item, isDir, err, name)
				}
			}

			lookup()
			beforeWarm := f.RequestStats()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range iterations {
				lookup()
			}
			runtime.ReadMemStats(&after)

			stats := f.RequestStats()
			if requests := stats.HTTPRequests - beforeWarm.HTTPRequests; requests != 0 {
				t.Fatalf("warm lookup requests = %d, want 0", requests)
			}
			if pages := stats.MetadataPages - beforeWarm.MetadataPages; pages != 0 {
				t.Fatalf("warm lookup pages = %d, want 0", pages)
			}
			if hits := stats.MetadataCacheHits - beforeWarm.MetadataCacheHits; hits != iterations {
				t.Fatalf("warm lookup cache hits = %d, want %d", hits, iterations)
			}
			if pages := fixture.pages.Load(); pages != uint64((count+999)/1000) {
				t.Fatalf("cold listing pages = %d, want %d", pages, (count+999)/1000)
			}

			bytesPerLookup := (after.TotalAlloc - before.TotalAlloc) / iterations
			if count == 1000 {
				smallBytes = bytesPerLookup
			}
			largestBytes = bytesPerLookup
			t.Logf("entries=%d warm_alloc_bytes/op=%d warm_requests/op=0 warm_pages/op=0", count, bytesPerLookup)
		})
	}

	if largestBytes > smallBytes+16*1024 {
		t.Fatalf("warm allocation grew from %d to %d bytes/op across generated scales", smallBytes, largestBytes)
	}
}

func TestLookupChildFreshStillLoadsCompleteDirectory(t *testing.T) {
	for _, count := range []int{1000, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f, fixture := newScalabilityFs(t, count, 1000)
			name := fmt.Sprintf("object-%08d", count-1)
			if _, _, _, err := f.lookupChild(context.Background(), "vault", name); err != nil {
				t.Fatalf("warm lookup: %v", err)
			}
			before := f.RequestStats()
			fixturePages := fixture.pages.Load()
			item, isDir, _, err := f.lookupChildFresh(context.Background(), "vault", name)
			if err != nil || item == nil || item.Name != name || isDir {
				t.Fatalf("fresh lookup = (%+v, dir=%t, err=%v), want file %q", item, isDir, err, name)
			}
			after := f.RequestStats()
			wantPages := uint64((count + 999) / 1000)
			if got := after.MetadataPages - before.MetadataPages; got != wantPages {
				t.Fatalf("fresh listing pages = %d, want %d", got, wantPages)
			}
			if got := fixture.pages.Load() - fixturePages; got != wantPages {
				t.Fatalf("fresh provider requests = %d, want %d", got, wantPages)
			}
		})
	}
}

func TestLookupChildIndexedBackupPreservesAcceptedHexEncoding(t *testing.T) {
	mock := newMockDegoo()
	backupName, err := newBackupName("z")
	if err != nil {
		t.Fatal(err)
	}
	backupName = strings.Replace(backupName, backupPrefix+"7a-", backupPrefix+"7A-", 1)
	mock.items["backup-target"] = mockItem{id: "backup-target", name: backupName, parentID: "vault", category: 6, size: 3}
	unrelated, err := newBackupName("other")
	if err != nil {
		t.Fatal(err)
	}
	mock.items["unrelated-backup"] = mockItem{id: "unrelated-backup", name: unrelated, parentID: "vault", category: 6, size: 4}
	f, server := newTestFs(t, mock)
	defer server.Close()
	for range 2 {
		item, isDir, _, err := f.lookupChild(context.Background(), "vault", "z")
		if err != nil || item == nil || item.ID != "backup-target" || isDir {
			t.Fatalf("accepted backup encoding was lost: item=%+v dir=%t err=%v", item, isDir, err)
		}
	}
}
