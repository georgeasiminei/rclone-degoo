package degoo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/dircache"
)

// scalabilityFixture generates pages on demand so the server does not retain a
// second copy of the directory being measured. All names and IDs are synthetic.
type scalabilityFixture struct {
	entries  int
	pageSize int
	pages    atomic.Uint64
	bytes    atomic.Uint64
	overlays atomic.Uint64
}

type scalabilityTokens struct{}

func (scalabilityTokens) Token(context.Context) (string, error) { return "generated-token", nil }
func (scalabilityTokens) Refresh(context.Context, string) (string, error) {
	return "generated-token", nil
}

func (fixture *scalabilityFixture) item(index int) map[string]any {
	return map[string]any{
		"ID": fmt.Sprintf("opaque-%08d", index), "Name": fmt.Sprintf("object-%08d", index),
		"ParentID": "vault", "Category": 6, "Size": "4096", "CreationTime": "1700000000000",
		"LastModificationTime": "1700000000000",
	}
}

func (fixture *scalabilityFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Operation string `json:"operationName"`
		Variables struct {
			ParentID  string
			Limit     int
			NextToken string
			ID        struct{ FileID string }
		} `json:"variables"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		http.Error(writer, "invalid fixture request", http.StatusBadRequest)
		return
	}
	var data any
	switch input.Operation {
	case "GetFileChildren5":
		if input.Variables.ParentID != "vault" || input.Variables.Limit != 1000 {
			http.Error(writer, "unexpected parent or requested page size", http.StatusBadRequest)
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
			items = append(items, fixture.item(index))
		}
		next := ""
		if end < fixture.entries {
			next = strconv.Itoa(end)
		}
		data = map[string]any{"getFileChildren5": map[string]any{"Items": items, "NextToken": next}}
		fixture.pages.Add(1)
	case "GetOverlay4":
		fixture.overlays.Add(1)
		target := fmt.Sprintf("opaque-%08d", fixture.entries-1)
		if input.Variables.ID.FileID == target {
			data = map[string]any{"getOverlay4": fixture.item(fixture.entries - 1)}
		} else {
			data = map[string]any{"getOverlay4": nil}
		}
	default:
		http.Error(writer, "unsupported fixture operation", http.StatusBadRequest)
		return
	}
	payload, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		http.Error(writer, "encode fixture response", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	fixture.bytes.Add(uint64(len(payload)))
	_, _ = writer.Write(payload)
}

func newScalabilityFs(t testing.TB, entries, pageSize int) (*Fs, *scalabilityFixture) {
	t.Helper()
	fixture := &scalabilityFixture{entries: entries, pageSize: pageSize}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	// Bootstrap/authentication is excluded to isolate directory-size costs.
	f := &Fs{name: "generated", opt: Options{RootFolderID: "vault"}, api: api.New(server.Client(), scalabilityTokens{}, server.URL, "generated-key")}
	f.dirCache = dircache.New("", "vault", f)
	return f, fixture
}

type scalabilityMeasurement struct {
	Entries       int    `json:"entries"`
	PageSize      int    `json:"fixture_page_size"`
	Operation     string `json:"operation"`
	DurationNS    int64  `json:"duration_ns"`
	Requests      uint64 `json:"requests"`
	Pages         uint64 `json:"pages"`
	ResponseBytes uint64 `json:"response_bytes"`
	CacheHits     uint64 `json:"cache_hits"`
	CacheMisses   uint64 `json:"cache_misses"`
}

func measureScalability(t *testing.T, f *Fs, fixture *scalabilityFixture, operation string, wantPages, wantHits uint64, run func() error) {
	t.Helper()
	before := f.api.RequestStats()
	beforeBytes := fixture.bytes.Load()
	beforePages := fixture.pages.Load()
	beforeOverlays := fixture.overlays.Load()
	started := time.Now()
	err := run()
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
	after := f.api.RequestStats()
	result := scalabilityMeasurement{
		Entries: fixture.entries, PageSize: fixture.pageSize, Operation: operation, DurationNS: elapsed.Nanoseconds(),
		Requests: after.HTTPRequests - before.HTTPRequests, Pages: after.MetadataPages - before.MetadataPages,
		ResponseBytes: fixture.bytes.Load() - beforeBytes,
		CacheHits:     after.MetadataCacheHits - before.MetadataCacheHits, CacheMisses: after.MetadataCacheMisses - before.MetadataCacheMisses,
	}
	if result.Pages != wantPages || fixture.pages.Load()-beforePages != wantPages || result.CacheHits != wantHits {
		t.Fatalf("%s counters = %+v; want pages=%d hits=%d", operation, result, wantPages, wantHits)
	}
	wantMisses := uint64(0)
	if wantPages > 0 {
		wantMisses = 1
	}
	if result.CacheMisses != wantMisses {
		t.Fatalf("%s cache misses = %d, want %d", operation, result.CacheMisses, wantMisses)
	}
	if operation == "direct_id" {
		if result.Requests != 1 || fixture.overlays.Load()-beforeOverlays != 1 {
			t.Fatalf("direct lookup counters = %+v", result)
		}
	} else if fixture.overlays.Load() != beforeOverlays || result.Requests != wantPages {
		t.Fatalf("%s requests = %d, want %d", operation, result.Requests, wantPages)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
}

func TestScalabilityDirectory(t *testing.T) {
	for _, count := range []int{1000, 10000, 100000} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			if count == 100000 && os.Getenv("RCLONE_DEGOO_SCALE_TESTS") != "1" {
				t.Skip("set RCLONE_DEGOO_SCALE_TESTS=1 for the 100,000-entry measurement")
			}
			f, fixture := newScalabilityFs(t, count, 1000)
			ctx := context.Background()
			name := fmt.Sprintf("object-%08d", count-1)
			lookup := func() error {
				obj, err := f.NewObject(ctx, name)
				if err == nil && (obj.Remote() != name || obj.Size() != 4096) {
					return fmt.Errorf("unexpected target: %v", obj)
				}
				return err
			}
			pages := uint64((count + fixture.pageSize - 1) / fixture.pageSize)
			measureScalability(t, f, fixture, "cold_lookup", pages, 0, lookup)
			measureScalability(t, f, fixture, "warm_lookup", 0, 1, lookup)
			measureScalability(t, f, fixture, "direct_id", 0, 0, func() error {
				item, err := f.api.GetItem(ctx, fmt.Sprintf("opaque-%08d", count-1))
				if err == nil && item.Name != name {
					return fmt.Errorf("wrong direct item: %q", item.Name)
				}
				return err
			})
			f.DirCacheFlush()
			list := func() error {
				entries, err := f.List(ctx, "")
				if err != nil {
					return err
				}
				if len(entries) != count {
					return fmt.Errorf("got %d entries, want %d", len(entries), count)
				}
				for index, entry := range entries {
					if entry.Remote() != fmt.Sprintf("object-%08d", index) {
						return fmt.Errorf("entry %d out of order or missing", index)
					}
					if _, ok := entry.(fs.Object); !ok {
						return fmt.Errorf("entry %d is not a file", index)
					}
				}
				return nil
			}
			measureScalability(t, f, fixture, "cold_list", pages, 0, list)
			measureScalability(t, f, fixture, "warm_list", 0, 1, list)
			// A refresh must invalidate the snapshot rather than hide provider work.
			f.DirCacheFlush()
			measureScalability(t, f, fixture, "lookup_after_refresh", pages, 0, lookup)
		})
	}
}

func BenchmarkScalabilityDirectory(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		for _, pageSize := range []int{1000, 100} {
			for _, operation := range []string{"cold_lookup", "warm_lookup", "cold_list", "warm_list", "direct_id"} {
				b.Run(fmt.Sprintf("entries=%d/page=%d/%s", count, pageSize, operation), func(b *testing.B) {
					f, fixture := newScalabilityFs(b, count, pageSize)
					ctx := context.Background()
					name := fmt.Sprintf("object-%08d", count-1)
					cold := operation == "cold_lookup" || operation == "cold_list"
					if !cold && operation != "direct_id" {
						if _, err := f.NewObject(ctx, name); err != nil {
							b.Fatal(err)
						}
					}
					before := f.api.RequestStats()
					beforeBytes := fixture.bytes.Load()
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						if cold {
							f.DirCacheFlush()
						}
						switch operation {
						case "cold_lookup", "warm_lookup":
							if _, err := f.NewObject(ctx, name); err != nil {
								b.Fatal(err)
							}
						case "cold_list", "warm_list":
							entries, err := f.List(ctx, "")
							if err != nil {
								b.Fatal(err)
							}
							if len(entries) != count {
								b.Fatalf("got %d entries, want %d", len(entries), count)
							}
						case "direct_id":
							if _, err := f.api.GetItem(ctx, fmt.Sprintf("opaque-%08d", count-1)); err != nil {
								b.Fatal(err)
							}
						}
					}
					b.StopTimer()
					after := f.api.RequestStats()
					wantRequests := uint64(0)
					if cold {
						wantRequests = uint64((count+pageSize-1)/pageSize) * uint64(b.N)
					}
					if operation == "direct_id" {
						wantRequests = uint64(b.N)
					}
					if actual := after.HTTPRequests - before.HTTPRequests; actual != wantRequests {
						b.Fatalf("requests = %d, want %d; warm snapshot may have expired", actual, wantRequests)
					}
					b.ReportMetric(float64(after.HTTPRequests-before.HTTPRequests)/float64(b.N), "requests/op")
					b.ReportMetric(float64(after.MetadataPages-before.MetadataPages)/float64(b.N), "pages/op")
					b.ReportMetric(float64(after.MetadataCacheHits-before.MetadataCacheHits)/float64(b.N), "cache-hits/op")
					b.ReportMetric(float64(fixture.bytes.Load()-beforeBytes)/float64(b.N), "response-B/op")
				})
			}
		}
	}
}
