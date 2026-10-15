package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/lib/pacer"
)

func TestGetItemRequestTerminationDoesNotListOrReportAbsence(t *testing.T) {
	for _, test := range []struct {
		name             string
		context          func() (context.Context, context.CancelFunc)
		wantError        error
		cancelAfterStart bool
		transportTimeout bool
	}{
		{
			name: "canceled",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			wantError:        context.Canceled,
			cancelAfterStart: true,
		},
		{
			name: "deadline exceeded",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			wantError:        context.DeadlineExceeded,
			transportTimeout: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requestStarted := make(chan struct{})
			var requests atomic.Int32
			transport := testRoundTripper(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				close(requestStarted)
				if test.transportTimeout {
					transportContext, cancel := context.WithTimeout(request.Context(), 20*time.Millisecond)
					defer cancel()
					<-transportContext.Done()
					return nil, transportContext.Err()
				}
				<-request.Context().Done()
				return nil, request.Context().Err()
			})
			client := New(&http.Client{Transport: transport}, &testTokens{initial: "generated-token"}, "https://graphql.invalid", "generated-api-key")
			ctx, cancel := test.context()
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := client.GetItem(ctx, "generated-target")
				result <- err
			}()

			select {
			case <-requestStarted:
			case <-time.After(3 * time.Second):
				t.Fatal("GetItem did not start its direct-ID request")
			}
			if test.cancelAfterStart {
				cancel()
			}

			select {
			case err := <-result:
				if !errors.Is(err, test.wantError) {
					t.Fatalf("GetItem termination error = %T %v, want %v", err, err, test.wantError)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("GetItem did not return after its request context ended")
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("direct-ID transport calls = %d, want one", got)
			}
			assertGetItemRequestCounts(t, client, 1)
		})
	}
}

func TestGetItemHTTPAndMalformedFailuresDoNotListOrReportAbsence(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		malformed   bool
		checkRetry  bool
		wantRetry   bool
		wantRetryIn time.Duration
	}{
		{name: "http 503", status: http.StatusServiceUnavailable, checkRetry: true, wantRetry: true, wantRetryIn: 2 * time.Second},
		{name: "malformed response", status: http.StatusOK, malformed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body := decodeGraphQLRequest(t, request)
				if body["operationName"] != "GetOverlay4" {
					t.Errorf("operation = %v, want GetOverlay4", body["operationName"])
				}
				if test.malformed {
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(`{"data":{"getOverlay4":`))
					return
				}
				writer.Header().Set("Retry-After", "2")
				writer.WriteHeader(test.status)
			}))
			defer server.Close()

			tokens := &testTokens{initial: "generated-token"}
			client := newTestClient(server, tokens)
			item, err := client.GetItem(context.Background(), "generated-target")
			var apiErr *Error
			if !errors.As(err, &apiErr) || item.ID != "" {
				t.Fatalf("GetItem failure returned item=%+v error=%T %v; want API error and no item", item, err, err)
			}
			if got := HTTPStatus(err); got != test.status && !test.malformed {
				t.Fatalf("HTTP status = %d, want %d", got, test.status)
			}
			if got := Retryable(err); test.checkRetry && got != test.wantRetry {
				t.Fatalf("Retryable = %t, want %t", got, test.wantRetry)
			}
			if test.wantRetryIn != 0 {
				if delay, ok := pacer.IsRetryAfter(err); !ok || delay != test.wantRetryIn {
					t.Fatalf("Retry-After = %v, %t; want %s", delay, ok, test.wantRetryIn)
				}
			}
			if tokens.calls.Load() != 1 {
				t.Fatalf("token calls = %d, want one", tokens.calls.Load())
			}
			assertGetItemRequestCounts(t, client, 1)
		})
	}
}

func TestGetItemHTTPUnauthorizedRefreshesWithoutListing(t *testing.T) {
	tokens := &testTokens{initial: "expired-generated-token", refreshed: "fresh-generated-token"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		if body["operationName"] != "GetOverlay4" {
			t.Errorf("operation = %v, want GetOverlay4", body["operationName"])
		}
		variables := body["variables"].(map[string]any)
		requests.Add(1)
		switch variables["Token"] {
		case "expired-generated-token":
			writer.WriteHeader(http.StatusUnauthorized)
		case "fresh-generated-token":
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getOverlay4": map[string]any{
				"ID": "generated-target", "Name": "generated-name", "ParentID": "generated-parent", "Category": 6,
			}}})
		default:
			t.Errorf("unexpected token class in direct-ID request")
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := newTestClient(server, tokens)
	item, err := client.GetItem(context.Background(), "generated-target")
	if err != nil {
		t.Fatalf("GetItem after HTTP 401 refresh: %v", err)
	}
	if item.ID != "generated-target" {
		t.Fatalf("GetItem ID = %q, want generated target", item.ID)
	}
	if requests.Load() != 2 || tokens.refreshes.Load() != 1 {
		t.Fatalf("direct requests/refreshes = %d/%d, want 2/1", requests.Load(), tokens.refreshes.Load())
	}
	assertGetItemRequestCounts(t, client, 2)
}

func TestGetItemRepeatedUnauthorizedIsBoundedAndDoesNotList(t *testing.T) {
	tokens := &testTokens{initial: "expired-generated-token", refreshed: "fresh-generated-token"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		if body["operationName"] != "GetOverlay4" {
			t.Errorf("operation = %v, want GetOverlay4", body["operationName"])
		}
		requests.Add(1)
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	client := newTestClient(server, tokens)
	item, err := client.GetItem(context.Background(), "generated-target")
	var apiErr *Error
	if !errors.As(err, &apiErr) || item.ID != "" {
		t.Fatalf("rejected GetItem returned item=%+v error=%T %v; want API error and no item", item, err, err)
	}
	if HTTPStatus(err) != http.StatusUnauthorized || Retryable(err) {
		t.Fatalf("repeated unauthorized status/retryable = %d/%t, want 401/false", HTTPStatus(err), Retryable(err))
	}
	if requests.Load() != 2 || tokens.refreshes.Load() != 1 {
		t.Fatalf("direct requests/refreshes = %d/%d, want two requests and one refresh", requests.Load(), tokens.refreshes.Load())
	}
	assertGetItemRequestCounts(t, client, 2)
}

func TestGetItemRateLimitDeadlinePersistsAndBlocksReloadedClient(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rateLimitFile := filepath.Join(privateDir, "auth.json.rate-limit.json")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Retry-After", "120")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	firstTokens := &testTokens{initial: "generated-token"}
	first := newTestClient(server, firstTokens)
	if err := first.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("first SetRateLimitFile: %v", err)
	}
	started := time.Now()
	_, err := first.GetItem(context.Background(), "generated-target")
	finished := time.Now()
	var firstRateErr *RateLimitError
	if !errors.As(err, &firstRateErr) || firstRateErr.Until.IsZero() {
		t.Fatalf("first GetItem error = %T %v, want a deadline-bearing rate-limit error", err, err)
	}
	savedUntil, err := readRateLimitFile(rateLimitFile)
	if err != nil {
		t.Fatalf("read persisted cooldown: %v", err)
	}
	if firstRateErr.Until.Before(started.Add(120*time.Second)) || firstRateErr.Until.After(finished.Add(120*time.Second)) {
		t.Fatalf("first cooldown deadline = %s, want about 120 seconds from now", firstRateErr.Until)
	}
	assertGetItemRequestCounts(t, first, 1)

	secondTokens := &testTokens{initial: "generated-token"}
	second := newTestClient(server, secondTokens)
	if err := second.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("second SetRateLimitFile: %v", err)
	}
	_, err = second.GetItem(context.Background(), "generated-target")
	var secondRateErr *RateLimitError
	if !errors.As(err, &secondRateErr) || !secondRateErr.Until.Equal(savedUntil) {
		t.Fatalf("reloaded GetItem error = %T %v; want persisted deadline %s", err, err, savedUntil)
	}
	if requests.Load() != 1 || secondTokens.calls.Load() != 0 {
		t.Fatalf("reloaded cooldown activity = requests %d, token calls %d; want 1/0", requests.Load(), secondTokens.calls.Load())
	}
	assertGetItemRequestCounts(t, second, 0)
}

func assertGetItemRequestCounts(t *testing.T, client *Client, want uint64) {
	t.Helper()
	stats := client.RequestStats()
	if stats.HTTPRequests != want || stats.RequestsByOperation["GetOverlay4"] != want {
		t.Fatalf("direct-ID request stats = %+v, want %d GetOverlay4 requests", stats, want)
	}
	if stats.MetadataPages != 0 || stats.RequestsByOperation["GetFileChildren5"] != 0 {
		t.Fatalf("direct-ID failure fell back to a listing: %+v", stats)
	}
}
