package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestListDirLaterPageRateLimitDiscardsPartialResultsAndBlocksCachedFolder(t *testing.T) {
	var requests atomic.Int32
	var largeFolderPages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		variables := body["variables"].(map[string]any)
		parentID := variables["ParentID"].(string)
		switch parentID {
		case "small-folder":
			requests.Add(1)
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
				"Items": []map[string]string{{"ID": "small-item", "Name": "item"}},
			}}})
		case "large-folder":
			requests.Add(1)
			page := largeFolderPages.Add(1)
			if page == 2 {
				if variables["NextToken"] != "later-page" {
					t.Errorf("second-page cursor = %v, want later-page", variables["NextToken"])
				}
				writer.Header().Set("Retry-After", "120")
				writer.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if variables["NextToken"] != nil {
				t.Errorf("first-page cursor = %v, want none", variables["NextToken"])
			}
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
				"Items":     []map[string]string{{"ID": "partial-item", "Name": "partial"}},
				"NextToken": "later-page",
			}}})
		default:
			t.Errorf("unexpected parent ID %q", parentID)
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := newTestClient(server, &testTokens{initial: "token"})
	if items, err := client.ListDir(context.Background(), "small-folder"); err != nil || len(items) != 1 || items[0].ID != "small-item" {
		t.Fatalf("small-folder ListDir = %#v, %v; want its independent successful listing", items, err)
	}

	started := time.Now()
	items, err := client.ListDir(context.Background(), "large-folder")
	finished := time.Now()
	if len(items) != 0 {
		t.Fatalf("partial large-folder results = %#v, want no items", items)
	}
	var rateErr *RateLimitError
	if !errors.As(err, &rateErr) {
		t.Fatalf("large-folder error = %T %v, want *RateLimitError", err, err)
	}
	if rateErr.Until.Before(started.Add(120*time.Second)) || rateErr.Until.After(finished.Add(120*time.Second)) {
		t.Fatalf("later-page Retry-After deadline = %v, want 120s after a time within the request interval", rateErr.Until)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("HTTP requests after later-page 429 = %d, want small-folder and two large-folder pages", got)
	}

	client.metadataMu.Lock()
	_, partialCached := client.metadataCache["large-folder"]
	_, smallCached := client.metadataCache["small-folder"]
	client.metadataMu.Unlock()
	if partialCached {
		t.Fatal("failed paginated listing left a partial large-folder cache entry")
	}
	if !smallCached {
		t.Fatal("successful small-folder listing was not cached before the cooldown")
	}

	items, err = client.ListDir(context.Background(), "small-folder")
	var blockedErr *RateLimitError
	if len(items) != 0 || !errors.As(err, &blockedErr) || !blockedErr.Until.Equal(rateErr.Until) {
		t.Fatalf("cached unrelated folder during cooldown = %#v, %v; want no items and the same global deadline", items, err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("HTTP requests after blocked cached lookup = %d, want 3", got)
	}
}

func TestListDirCancellationDuringLaterPageDiscardsPartialResults(t *testing.T) {
	var requests atomic.Int32
	pageTwoStarted := make(chan struct{}, 1)
	pageTwoCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		variables := body["variables"].(map[string]any)
		page := requests.Add(1)
		if page == 1 {
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
				"Items":     []map[string]string{{"ID": "partial-item", "Name": "partial"}},
				"NextToken": "later-page",
			}}})
			return
		}
		if variables["NextToken"] != "later-page" {
			t.Errorf("second-page cursor = %v, want later-page", variables["NextToken"])
		}
		pageTwoStarted <- struct{}{}
		select {
		case <-request.Context().Done():
			close(pageTwoCanceled)
		case <-time.After(3 * time.Second):
			t.Error("client did not cancel the in-flight second-page request")
		}
	}))
	defer server.Close()

	client := newTestClient(server, &testTokens{initial: "token"})
	ctx, cancel := context.WithCancel(context.Background())
	type listResult struct {
		items []Item
		err   error
	}
	result := make(chan listResult, 1)
	go func() {
		items, err := client.ListDir(ctx, "large-folder")
		result <- listResult{items: items, err: err}
	}()

	select {
	case <-pageTwoStarted:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("listing did not reach the second page")
	}
	cancel()

	var got listResult
	select {
	case got = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("ListDir did not return after its context was canceled")
	}
	if len(got.items) != 0 || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled ListDir = %#v, %v; want no results and context.Canceled", got.items, got.err)
	}
	select {
	case <-pageTwoCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled listing did not stop the in-flight second-page request")
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("HTTP page requests = %d, want first page and canceled second page", got)
	}

	client.metadataMu.Lock()
	_, cached := client.metadataCache["large-folder"]
	client.metadataMu.Unlock()
	if cached {
		t.Fatal("canceled paginated listing left a partial cache entry")
	}
}
