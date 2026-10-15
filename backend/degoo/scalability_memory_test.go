package degoo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

// scalabilityMemoryEvidence records retained heap separately from cumulative
// allocation. The latter includes HTTP fixture, JSON, and backend work.
type scalabilityMemoryEvidence struct {
	Entries                       int    `json:"entries"`
	PageSize                      int    `json:"fixture_page_size"`
	Pages                         uint64 `json:"pages"`
	Requests                      uint64 `json:"requests"`
	ResponseBytes                 uint64 `json:"response_bytes"`
	ExpectedSnapshotParents       int    `json:"expected_snapshot_parents"`
	ExpectedSnapshotEntries       int    `json:"expected_snapshot_entries"`
	ColdListDurationNS            int64  `json:"cold_list_duration_ns"`
	HeapAllocBaselineAfterGCBytes uint64 `json:"heap_alloc_baseline_after_gc_bytes"`
	HeapAllocAfterListBytes       uint64 `json:"heap_alloc_after_list_before_gc_bytes"`
	HeapAllocAfterGCBytes         uint64 `json:"heap_alloc_after_gc_bytes"`
	RetainedHeapDeltaBytes        int64  `json:"retained_heap_delta_bytes"`
	CumulativeAllocBytes          uint64 `json:"cumulative_alloc_bytes_during_cold_list"`
	CumulativeMallocs             uint64 `json:"cumulative_mallocs_during_cold_list"`
	RetainedHeapObjectsDelta      int64  `json:"retained_heap_objects_delta"`
	RetainedHeapScope             string `json:"retained_heap_scope"`
	SampledPeakHeapAllocBytes     uint64 `json:"sampled_peak_heap_alloc_bytes_during_cold_list"`
	SampledPeakHeapDeltaBytes     int64  `json:"sampled_peak_heap_delta_bytes_during_cold_list"`
	HeapSamples                   uint64 `json:"heap_samples_during_cold_list"`
	HeapSampleIntervalNS          int64  `json:"heap_sample_interval_ns"`
	SampledPeakHeapScope          string `json:"sampled_peak_heap_scope"`
	WarmCacheHits                 uint64 `json:"warm_cache_hits"`
	WarmRequests                  uint64 `json:"warm_requests"`
}

type sampledHeapPeak struct {
	allocBytes uint64
	samples    uint64
}

// startSampledHeapPeak records process HeapAlloc periodically until stopped.
func startSampledHeapPeak(interval time.Duration) func() sampledHeapPeak {
	stop := make(chan struct{})
	ready := make(chan struct{})
	done := make(chan sampledHeapPeak, 1)
	go func() {
		var peak sampledHeapPeak
		sample := func() {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			if stats.HeapAlloc > peak.allocBytes {
				peak.allocBytes = stats.HeapAlloc
			}
			peak.samples++
		}
		sample()
		close(ready)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-stop:
				sample()
				done <- peak
				return
			}
		}
	}()
	<-ready
	return func() sampledHeapPeak {
		close(stop)
		return <-done
	}
}

func TestScalabilityRetainedMetadataMemory(t *testing.T) {
	for _, count := range []int{1000, 10000, 100000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			if count == 100000 && os.Getenv("RCLONE_DEGOO_SCALE_TESTS") != "1" {
				t.Skip("set RCLONE_DEGOO_SCALE_TESTS=1 for the 100,000-entry retained-memory measurement")
			}

			f, fixture := newScalabilityFs(t, count, 1000)
			ctx := context.Background()
			// Establish the local HTTP connection before the baseline without
			// populating either metadata cache.
			if _, err := f.api.ListDir(ctx, "warm-up"); err == nil {
				t.Fatal("invalid warm-up parent unexpectedly succeeded")
			}

			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			statsBefore := f.api.RequestStats()
			bytesBefore := fixture.bytes.Load()
			pagesBefore := fixture.pages.Load()

			const sampleInterval = 500 * time.Microsecond
			stopHeapSampler := startSampledHeapPeak(sampleInterval)
			started := time.Now()
			items, err := f.api.ListDir(ctx, "vault")
			elapsed := time.Since(started)
			sampledPeak := stopHeapSampler()
			if err != nil {
				t.Fatalf("cold directory listing: %v", err)
			}
			if len(items) != count || items[0].Name != "object-00000000" || items[count-1].Name != fmt.Sprintf("object-%08d", count-1) {
				t.Fatalf("listing has %d entries or unexpected boundary items", len(items))
			}

			var afterList runtime.MemStats
			runtime.ReadMemStats(&afterList)
			allocBytes := afterList.TotalAlloc - before.TotalAlloc
			allocs := afterList.Mallocs - before.Mallocs

			// The result slices are unused after their checks. The API snapshot
			// and bounded parent hints remain reachable through f.api.
			warmItems, err := f.api.ListDir(ctx, "vault")
			if err != nil {
				t.Fatalf("warm directory listing: %v", err)
			}
			if len(warmItems) != count {
				t.Fatalf("warm listing has %d entries, want %d", len(warmItems), count)
			}
			runtime.GC()
			var afterGC runtime.MemStats
			runtime.ReadMemStats(&afterGC)
			runtime.KeepAlive(f)

			statsAfter := f.api.RequestStats()
			pages := fixture.pages.Load() - pagesBefore
			requests := statsAfter.HTTPRequests - statsBefore.HTTPRequests
			warmHits := statsAfter.MetadataCacheHits - statsBefore.MetadataCacheHits
			warmRequests := requests - pages
			wantPages := uint64((count + fixture.pageSize - 1) / fixture.pageSize)
			if pages != wantPages || requests != wantPages || warmHits != 1 || warmRequests != 0 {
				t.Fatalf("listing counters: pages=%d requests=%d warm_hits=%d warm_requests=%d; want pages/requests=%d, one warm hit, and no warm requests", pages, requests, warmHits, warmRequests, wantPages)
			}

			evidence := scalabilityMemoryEvidence{
				Entries: count, PageSize: fixture.pageSize, Pages: pages, Requests: requests,
				ResponseBytes: fixture.bytes.Load() - bytesBefore, ExpectedSnapshotParents: 1,
				ExpectedSnapshotEntries: count, ColdListDurationNS: elapsed.Nanoseconds(),
				HeapAllocBaselineAfterGCBytes: before.HeapAlloc, HeapAllocAfterListBytes: afterList.HeapAlloc,
				HeapAllocAfterGCBytes:  afterGC.HeapAlloc,
				RetainedHeapDeltaBytes: int64(afterGC.HeapAlloc) - int64(before.HeapAlloc),
				CumulativeAllocBytes:   allocBytes, CumulativeMallocs: allocs,
				RetainedHeapObjectsDelta:  int64(afterGC.HeapObjects) - int64(before.HeapObjects),
				RetainedHeapScope:         "whole test-process heap delta after GC; includes the live fixture, HTTP transport, and test bookkeeping",
				SampledPeakHeapAllocBytes: sampledPeak.allocBytes,
				SampledPeakHeapDeltaBytes: int64(sampledPeak.allocBytes) - int64(before.HeapAlloc),
				HeapSamples:               sampledPeak.samples,
				HeapSampleIntervalNS:      sampleInterval.Nanoseconds(),
				SampledPeakHeapScope:      "sampled lower bound on whole test-process HeapAlloc during cold listing, including the live fixture, HTTP transport, and test bookkeeping; 500us polling can miss transient peaks and does not measure RSS or cache-only bytes",
				WarmCacheHits:             warmHits, WarmRequests: warmRequests,
			}
			if sampledPeak.samples < 2 || sampledPeak.allocBytes < before.HeapAlloc {
				t.Fatalf("sampled heap peak = %+v, baseline heap = %d; want at least two samples and a peak no lower than baseline", sampledPeak, before.HeapAlloc)
			}
			encoded, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			t.Log(string(encoded))
		})
	}
}
