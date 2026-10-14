package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/auth"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/stretchr/testify/require"
)

type testTokens struct {
	initial   string
	refreshed string
	refreshes atomic.Int32
	calls     atomic.Int32
}

func (tokens *testTokens) Token(context.Context) (string, error) {
	tokens.calls.Add(1)
	if tokens.refreshes.Load() != 0 && tokens.refreshed != "" {
		return tokens.refreshed, nil
	}
	return tokens.initial, nil
}

func (tokens *testTokens) Refresh(_ context.Context, previous string) (string, error) {
	if previous != tokens.initial {
		return "", fmt.Errorf("unexpected previous token %q", previous)
	}
	tokens.refreshes.Add(1)
	return tokens.refreshed, nil
}

func newTestClient(server *httptest.Server, tokens TokenSource) *Client {
	return New(server.Client(), tokens, server.URL, "test-api-key")
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip testRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func decodeGraphQLRequest(t *testing.T, request *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatalf("decode GraphQL request: %v", err)
	}
	return body
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode test response: %v", err)
	}
}

func TestGraphQLRefreshesUnauthorizedEnvelopeOnce(t *testing.T) {
	tokens := &testTokens{initial: "expired", refreshed: "fresh"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("x-api-key"); got != "test-api-key" {
			t.Errorf("x-api-key = %q, want test-api-key", got)
		}
		body := decodeGraphQLRequest(t, request)
		if body == nil {
			return
		}
		variables := body["variables"].(map[string]any)
		requests.Add(1)
		if variables["Token"] == "expired" {
			writeJSON(t, writer, map[string]any{"errors": []any{map[string]any{
				"message":   "Unauthorized",
				"errorType": "UnauthorizedException",
			}}})
			return
		}
		if variables["Token"] != "fresh" {
			t.Errorf("request token = %v, want fresh", variables["Token"])
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getOverlay4": map[string]any{
			"ID": "17", "Name": "example.txt", "ParentID": "3", "Category": 6,
			"Size": "42", "URL": "https://download.invalid/file", "LastModificationTime": 1700000000123,
			"CreationTime": "1700000000000",
		}}})
	}))
	defer server.Close()

	item, err := newTestClient(server, tokens).GetItem(context.Background(), "17")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.ID != "17" || item.Size != 42 || item.Category != 6 || item.Name != "example.txt" {
		t.Fatalf("unexpected item: %+v", item)
	}
	if item.LastModificationTime.UnixMilli() != 1700000000123 || item.CreationTime.UnixMilli() != 1700000000000 {
		t.Fatalf("timestamps were not decoded as milliseconds: %+v", item)
	}
	if requests.Load() != 2 || tokens.refreshes.Load() != 1 {
		t.Fatalf("requests = %d, refreshes = %d; want 2 and 1", requests.Load(), tokens.refreshes.Load())
	}
}

func TestItemUnmarshalJSONRFC3339Timestamps(t *testing.T) {
	const raw = `{"CreationTime":%q,"LastModificationTime":%q}`
	utc := time.Date(2026, time.October, 10, 12, 34, 56, 123456789, time.UTC)
	tests := []struct {
		name     string
		created  string
		modified string
	}{
		{
			name:     "UTC",
			created:  "2026-10-10T12:34:56.123456789Z",
			modified: "2026-10-10T12:34:56.123Z",
		},
		{
			name:     "timezone offsets",
			created:  "2026-10-10T15:34:56.123456789+03:00",
			modified: "2026-10-10T08:34:56.123-04:00",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := []byte(fmt.Sprintf(raw, test.created, test.modified))
			var item Item
			if err := json.Unmarshal(data, &item); err != nil {
				t.Fatalf("unmarshal Item: %v", err)
			}
			wantCreated, err := time.Parse(time.RFC3339Nano, test.created)
			if err != nil {
				t.Fatalf("parse expected creation time: %v", err)
			}
			wantModified, err := time.Parse(time.RFC3339Nano, test.modified)
			if err != nil {
				t.Fatalf("parse expected modification time: %v", err)
			}
			if !item.CreationTime.Equal(wantCreated) || !item.LastModificationTime.Equal(wantModified) {
				t.Fatalf("decoded times = %s/%s, want %s/%s", item.CreationTime, item.LastModificationTime, wantCreated, wantModified)
			}
			if test.name == "UTC" && !item.CreationTime.Equal(utc) {
				t.Fatalf("creation time = %s, want %s", item.CreationTime, utc)
			}
		})
	}
}

func TestParseMillisZeroAndInvalidValues(t *testing.T) {
	for _, value := range []string{"", "null", `""`, "0", `"0"`} {
		got, err := parseMillis(json.RawMessage(value))
		if err != nil || !got.IsZero() {
			t.Errorf("parseMillis(%q) = %s, %v; want zero, nil", value, got, err)
		}
	}
	for _, value := range []string{`"not-a-timestamp"`, "true", "1.5"} {
		if got, err := parseMillis(json.RawMessage(value)); err == nil {
			t.Errorf("parseMillis(%q) = %s, nil; want error", value, got)
		}
	}
}

func TestGraphQLDoesNotFollowCrossHostRedirect(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var destinationRequests atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				destinationRequests.Add(1)
				writeJSON(t, writer, map[string]any{"data": map[string]any{"getOverlay4": map[string]any{"ID": "17"}}})
			}))
			defer destination.Close()

			const secretToken = "redirected-token-secret"
			origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if got := request.Header.Get("x-api-key"); got != "test-api-key" {
					t.Errorf("origin x-api-key = %q, want test-api-key", got)
				}
				body := decodeGraphQLRequest(t, request)
				variables := body["variables"].(map[string]any)
				if variables["Token"] != secretToken {
					t.Errorf("origin token = %v, want secret token", variables["Token"])
				}
				writer.Header().Set("Location", destination.URL+"/graphql")
				writer.WriteHeader(status)
			}))
			defer origin.Close()

			_, err := newTestClient(origin, &testTokens{initial: secretToken}).GetItem(context.Background(), "17")
			if err == nil {
				t.Fatal("GetItem unexpectedly followed redirect and succeeded")
			}
			if got := destinationRequests.Load(); got != 0 {
				t.Fatalf("redirect destination received %d requests, want zero", got)
			}
		})
	}
}

func TestMutationRefreshRequiresOnlyUnauthorizedErrorsAndNoData(t *testing.T) {
	tests := []struct {
		name   string
		data   any
		errors []any
	}{
		{
			name: "partial data and unauthorized error",
			data: map[string]any{"setUploadFile3": "registered"},
			errors: []any{map[string]any{
				"message": "Unauthorized", "errorType": "UnauthorizedException",
			}},
		},
		{
			name: "mixed unauthorized and other errors",
			data: nil,
			errors: []any{
				map[string]any{"message": "Unauthorized", "errorType": "UnauthorizedException"},
				map[string]any{"message": "mutation failed", "errorType": "ValidationException"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tokens := &testTokens{initial: "expired", refreshed: "fresh"}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				writeJSON(t, writer, map[string]any{"data": test.data, "errors": test.errors})
			}))
			defer server.Close()

			_, err := newTestClient(server, tokens).Register(context.Background(), "parent", "file.bin", "checksum", 1, time.Now())
			if err == nil {
				t.Fatal("Register unexpectedly succeeded")
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("Register sent %d requests, want one", got)
			}
			if got := tokens.refreshes.Load(); got != 0 {
				t.Fatalf("Register refreshed token %d times, want zero", got)
			}
		})
	}
}

func TestMutationAcknowledgements(t *testing.T) {
	operations := []struct {
		name          string
		operation     string
		field         string
		returnsString bool
		invoke        func(*Client) (string, error)
	}{
		{
			name:          "Register",
			operation:     "SetUploadFile3",
			field:         "setUploadFile3",
			returnsString: true,
			invoke: func(client *Client) (string, error) {
				return client.Register(context.Background(), "parent", "file.txt", "checksum", 1, time.Now())
			},
		},
		{
			name:          "Mkdir",
			operation:     "SetUploadFile3",
			field:         "setUploadFile3",
			returnsString: true,
			invoke: func(client *Client) (string, error) {
				return client.Mkdir(context.Background(), "parent", "folder")
			},
		},
		{
			name:      "Rename",
			operation: "SetRenameFile",
			field:     "setRenameFile",
			invoke: func(client *Client) (string, error) {
				return "", client.Rename(context.Background(), "id", "new-name")
			},
		},
		{
			name:      "Move",
			operation: "SetMoveFile",
			field:     "setMoveFile",
			invoke: func(client *Client) (string, error) {
				return "", client.Move(context.Background(), "id", "new-parent")
			},
		},
		{
			name:      "Delete",
			operation: "SetDeleteFile5",
			field:     "setDeleteFile5",
			invoke: func(client *Client) (string, error) {
				return "", client.Delete(context.Background(), "id", false)
			},
		},
	}
	cases := []struct {
		name          string
		value         any
		omit          bool
		wantErr       bool
		negativeAck   bool
		wantErrorText string
		wantString    string
	}{
		{name: "boolean true", value: true},
		{name: "legacy string", value: "legacy result", wantString: "legacy result"},
		{name: "false", value: false, wantErr: true, negativeAck: true, wantErrorText: "mutation acknowledgement was false; outcome is ambiguous"},
		{name: "null", value: nil, wantErr: true, wantErrorText: "mutation acknowledgement was null; outcome is ambiguous"},
		{name: "wrong type", value: 42, wantErr: true, wantErrorText: "response contained an invalid mutation acknowledgement"},
		{name: "missing", omit: true, wantErr: true, wantErrorText: "response omitted the mutation acknowledgement; outcome is ambiguous"},
	}
	for _, operation := range operations {
		for _, test := range cases {
			t.Run(operation.name+"/"+test.name, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					data := map[string]any{}
					if !test.omit {
						data[operation.field] = test.value
					}
					writeJSON(t, writer, map[string]any{"data": data})
				}))
				defer server.Close()

				got, err := operation.invoke(newTestClient(server, &testTokens{initial: "token"}))
				if test.wantErr {
					if err == nil {
						t.Fatalf("%s unexpectedly succeeded with %v acknowledgement", operation.name, test.name)
					}
					if got, want := err.Error(), operation.operation+": "+test.wantErrorText; got != want {
						t.Fatalf("%s error = %q, want %q", operation.name, got, want)
					}
					if got := errors.Is(err, ErrNegativeAcknowledgement); got != test.negativeAck {
						t.Fatalf("%s negative acknowledgement = %t, want %t", operation.name, got, test.negativeAck)
					}
					if got := requests.Load(); got != 1 {
						t.Fatalf("%s sent %d requests for an unacknowledged mutation, want one", operation.name, got)
					}
					return
				}
				wantString := test.wantString
				if !operation.returnsString {
					wantString = ""
				}
				if err != nil || got != wantString {
					t.Fatalf("%s = %q, %v; want %q, nil", operation.name, got, err, wantString)
				}
			})
		}
	}
}

func TestRegisterDoesNotReplayUnauthorizedMutation(t *testing.T) {
	tokens := &testTokens{initial: "expired", refreshed: "fresh"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = decodeGraphQLRequest(t, request)
		requests.Add(1)
		writeJSON(t, writer, map[string]any{"errors": []any{map[string]any{
			"message": "Unauthorized", "errorType": "UnauthorizedException",
		}}})
	}))
	defer server.Close()

	got, err := newTestClient(server, tokens).Register(context.Background(), "parent", "file.bin", "checksum", 1, time.Now())
	if err == nil || got != "" {
		t.Fatalf("Register = %q, %v; want an ambiguous mutation error", got, err)
	}
	if requests.Load() != 1 || tokens.refreshes.Load() != 0 {
		t.Fatalf("requests/refreshes = %d/%d, want 1/0", requests.Load(), tokens.refreshes.Load())
	}
}

func TestListDirPaginatesOverThousandItems(t *testing.T) {
	tokens := &testTokens{initial: "token"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		if body == nil {
			return
		}
		variables := body["variables"].(map[string]any)
		if variables["Limit"] != float64(maxLimit) {
			t.Errorf("Limit = %v, want %d", variables["Limit"], maxLimit)
		}
		page := requests.Add(1)
		items := make([]map[string]any, 0)
		if page == 1 {
			for i := 0; i < maxLimit; i++ {
				items = append(items, map[string]any{"ID": strconv.Itoa(i + 1), "Name": "item"})
			}
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": items, "NextToken": "opaque-cursor"}}})
			return
		}
		if variables["NextToken"] != "opaque-cursor" {
			t.Errorf("NextToken = %v, want opaque-cursor", variables["NextToken"])
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": []map[string]any{{"ID": "1001", "Name": "last"}}}}})
	}))
	defer server.Close()

	items, err := newTestClient(server, tokens).ListDir(context.Background(), "parent")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(items) != 1001 || items[0].ID != "1" || items[1000].ID != "1001" || requests.Load() != 2 {
		t.Fatalf("unexpected result: items=%d requests=%d", len(items), requests.Load())
	}
}

func TestListDirRejectsDuplicateIDsAcrossPages(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		page := requests.Add(1)
		result := map[string]any{"Items": []map[string]string{{"ID": "duplicate", "Name": "item"}}}
		if page == 1 {
			result["NextToken"] = "next"
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": result}})
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: "token"}).ListDir(context.Background(), "parent")
	if err == nil || !strings.Contains(err.Error(), `duplicate item ID "duplicate"`) {
		t.Fatalf("ListDir error = %v, want duplicate ID error", err)
	}
}

func TestListDirUsesShortLivedDefensiveCache(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
			"Items": []map[string]string{{"ID": "item", "Name": "original"}},
		}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})

	first, err := client.ListDir(context.Background(), "parent")
	if err != nil {
		t.Fatalf("first ListDir: %v", err)
	}
	first[0].Name = "caller mutation"
	second, err := client.ListDir(context.Background(), "parent")
	if err != nil {
		t.Fatalf("second ListDir: %v", err)
	}
	if second[0].Name != "original" {
		t.Fatalf("cached item after caller changed prior result = %q, want original", second[0].Name)
	}
	second[0].Name = "second caller mutation"
	third, err := client.ListDir(context.Background(), "parent")
	if err != nil {
		t.Fatalf("third ListDir: %v", err)
	}
	if third[0].Name != "original" || requests.Load() != 1 {
		t.Fatalf("third result/request count = %q/%d, want original/1", third[0].Name, requests.Load())
	}

	client.metadataMu.Lock()
	entry, ok := client.metadataCache["parent"]
	client.metadataMu.Unlock()
	if !ok {
		t.Fatal("successful ListDir did not populate metadata cache")
	}
	remaining := time.Until(entry.expires)
	if remaining <= 0 || remaining > metadataCacheTTL {
		t.Fatalf("cache lifetime remaining = %v, want (0, %v]", remaining, metadataCacheTTL)
	}
}

func TestListDirCacheExpiryAndExplicitClear(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		page := requests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
			"Items": []map[string]string{{"ID": strconv.Itoa(int(page)), "Name": "item"}},
		}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})

	if _, err := client.ListDir(context.Background(), "parent"); err != nil {
		t.Fatalf("initial ListDir: %v", err)
	}
	client.metadataMu.Lock()
	entry := client.metadataCache["parent"]
	entry.expires = time.Now().Add(-time.Second)
	client.metadataCache["parent"] = entry
	client.metadataMu.Unlock()
	if _, err := client.ListDir(context.Background(), "parent"); err != nil {
		t.Fatalf("ListDir after expiry: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests after expiry = %d, want 2", got)
	}

	client.ClearMetadataCache()
	if _, err := client.ListDir(context.Background(), "parent"); err != nil {
		t.Fatalf("ListDir after explicit clear: %v", err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests after explicit clear = %d, want 3", got)
	}
}

func TestListDirFreshBypassesAndInvalidatesCache(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		page := requests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
			"Items": []map[string]string{{"ID": strconv.Itoa(int(page)), "Name": "item"}},
		}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})

	initial, err := client.ListDir(context.Background(), "parent")
	if err != nil || initial[0].ID != "1" {
		t.Fatalf("initial ListDir = %#v, %v; want item 1", initial, err)
	}
	fresh, err := client.ListDirFresh(context.Background(), "parent")
	if err != nil || fresh[0].ID != "2" {
		t.Fatalf("ListDirFresh = %#v, %v; want item 2", fresh, err)
	}
	current, err := client.ListDir(context.Background(), "parent")
	if err != nil || current[0].ID != "3" {
		t.Fatalf("ListDir after fresh read = %#v, %v; want item 3", current, err)
	}
	cached, err := client.ListDir(context.Background(), "parent")
	if err != nil || cached[0].ID != "3" || requests.Load() != 3 {
		t.Fatalf("cached ListDir = %#v, %v with %d requests; want item 3 and 3 requests", cached, err, requests.Load())
	}
}

func TestListDirCoalescesConcurrentMisses(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
			"Items": []map[string]string{{"ID": "item", "Name": "file", "ParentID": "parent"}},
		}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})

	const waiters = 8
	results := make(chan error, waiters)
	for range waiters {
		go func() {
			items, err := client.ListDir(context.Background(), "parent")
			if err == nil && (len(items) != 1 || items[0].ID != "item") {
				err = fmt.Errorf("unexpected items: %#v", items)
			}
			results <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("ListDir request did not reach server")
	}
	waitForRequestStat(t, client, func(stats RequestStats) bool { return stats.MetadataCacheCoalesced == waiters-1 })
	close(release)
	for range waiters {
		if err := <-results; err != nil {
			t.Fatalf("ListDir: %v", err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("provider requests = %d, want 1", requests.Load())
	}
	stats := client.RequestStats()
	if stats.MetadataCacheMisses != waiters || stats.MetadataCacheCoalesced != waiters-1 || stats.MetadataPages != 1 {
		t.Fatalf("metadata stats = %+v, want %d misses, %d coalesced waiters, one page", stats, waiters, waiters-1)
	}
}

func TestListDirWaiterCancellationDoesNotCancelSharedRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		close(started)
		<-release
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": []any{}}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := client.ListDir(firstCtx, "parent")
		firstResult <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first ListDir request did not reach server")
	}
	secondResult := make(chan error, 1)
	go func() {
		_, err := client.ListDir(context.Background(), "parent")
		secondResult <- err
	}()
	waitForRequestStat(t, client, func(stats RequestStats) bool { return stats.MetadataCacheCoalesced == 1 })
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first ListDir error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-secondResult; err != nil {
		t.Fatalf("second ListDir: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("provider requests = %d, want 1", requests.Load())
	}
}

func TestListDirCancelsSharedRequestWhenAllWaitersLeave(t *testing.T) {
	started := make(chan struct{})
	requestCanceled := make(chan struct{})
	client := New(&http.Client{Transport: testRoundTripper(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		close(requestCanceled)
		return nil, request.Context().Err()
	})}, &testTokens{initial: "token"}, "https://api.invalid/graphql", "test-api-key")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.ListDir(ctx, "parent")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("ListDir request did not reach server")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("ListDir error = %v, want context.Canceled", err)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("shared HTTP request was not canceled after its last waiter left")
	}
}

func TestListDirFreshCoalescesAndInvalidatesOnlyItsParent(t *testing.T) {
	type parentCounter struct {
		count int
	}
	var mu sync.Mutex
	counters := map[string]*parentCounter{}
	freshStarted := make(chan struct{})
	releaseFresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		variables := body["variables"].(map[string]any)
		parentID := variables["ParentID"].(string)
		mu.Lock()
		counter := counters[parentID]
		if counter == nil {
			counter = &parentCounter{}
			counters[parentID] = counter
		}
		counter.count++
		count := counter.count
		mu.Unlock()
		if parentID == "a" && count == 2 {
			close(freshStarted)
			<-releaseFresh
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
			"Items": []map[string]string{{"ID": fmt.Sprintf("%s-%d", parentID, count), "Name": "file", "ParentID": parentID}},
		}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})
	for _, parentID := range []string{"a", "b"} {
		if _, err := client.ListDir(context.Background(), parentID); err != nil {
			t.Fatalf("prime %s: %v", parentID, err)
		}
	}

	results := make(chan error, 2)
	for range 2 {
		go func() {
			items, err := client.ListDirFresh(context.Background(), "a")
			if err == nil && (len(items) != 1 || items[0].ID != "a-2") {
				err = fmt.Errorf("unexpected fresh items: %#v", items)
			}
			results <- err
		}()
	}
	select {
	case <-freshStarted:
	case <-time.After(time.Second):
		t.Fatal("fresh listing did not reach server")
	}
	waitForRequestStat(t, client, func(stats RequestStats) bool { return stats.MetadataCacheCoalesced == 1 })
	close(releaseFresh)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("ListDirFresh: %v", err)
		}
	}
	items, err := client.ListDir(context.Background(), "b")
	if err != nil || len(items) != 1 || items[0].ID != "b-1" {
		t.Fatalf("ListDir b = %#v, %v; want cached b-1", items, err)
	}
	mu.Lock()
	gotA, gotB := counters["a"].count, counters["b"].count
	mu.Unlock()
	if gotA != 2 || gotB != 1 {
		t.Fatalf("parent listing requests = a:%d b:%d, want a:2 b:1", gotA, gotB)
	}
}

func TestTrackedRenameInvalidatesOnlyItsParent(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		switch body["operationName"] {
		case "GetFileChildren5":
			parentID := body["variables"].(map[string]any)["ParentID"].(string)
			mu.Lock()
			counts[parentID]++
			mu.Unlock()
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
				"Items": []map[string]string{{"ID": "item-" + parentID, "Name": "file", "ParentID": parentID}},
			}}})
		case "SetRenameFile":
			writeJSON(t, writer, map[string]any{"data": map[string]any{"setRenameFile": true}})
		default:
			t.Errorf("unexpected operation %v", body["operationName"])
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})
	for _, parentID := range []string{"a", "b"} {
		if _, err := client.ListDir(context.Background(), parentID); err != nil {
			t.Fatalf("prime %s: %v", parentID, err)
		}
	}
	if err := client.Rename(context.Background(), "item-a", "renamed"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := client.ListDir(context.Background(), "a"); err != nil {
		t.Fatalf("ListDir a after rename: %v", err)
	}
	if _, err := client.ListDir(context.Background(), "b"); err != nil {
		t.Fatalf("ListDir b after rename: %v", err)
	}
	mu.Lock()
	gotA, gotB := counts["a"], counts["b"]
	mu.Unlock()
	stats := client.RequestStats()
	if gotA != 2 || gotB != 1 || stats.MetadataGlobalInvalidations != 0 {
		t.Fatalf("listing counts a:%d b:%d, global invalidations %d; want 2/1/0", gotA, gotB, stats.MetadataGlobalInvalidations)
	}
}

func TestStaleItemLookupCannotOverwriteMovedItemParent(t *testing.T) {
	var mu sync.Mutex
	listingCounts := map[string]int{}
	var overlayRequests atomic.Int32
	staleLookupStarted := make(chan struct{})
	releaseStaleLookup := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		switch body["operationName"] {
		case "GetOverlay4":
			if overlayRequests.Add(1) == 2 {
				close(staleLookupStarted)
				<-releaseStaleLookup
			}
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getOverlay4": map[string]any{
				"ID": "item", "Name": "file", "ParentID": "old", "Category": 1, "Size": "1", "URL": "https://storage.invalid/object",
			}}})
		case "GetFileChildren5":
			parentID := body["variables"].(map[string]any)["ParentID"].(string)
			mu.Lock()
			listingCounts[parentID]++
			mu.Unlock()
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": []any{}}}})
		case "SetMoveFile":
			writeJSON(t, writer, map[string]any{"data": map[string]any{"setMoveFile": true}})
		case "SetRenameFile":
			writeJSON(t, writer, map[string]any{"data": map[string]any{"setRenameFile": true}})
		default:
			t.Errorf("unexpected operation %v", body["operationName"])
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})
	for _, parentID := range []string{"new", "unrelated"} {
		if _, err := client.ListDir(context.Background(), parentID); err != nil {
			t.Fatalf("prime %s: %v", parentID, err)
		}
	}
	if _, err := client.GetItem(context.Background(), "item"); err != nil {
		t.Fatalf("initial GetItem: %v", err)
	}

	staleLookup := make(chan error, 1)
	go func() {
		_, err := client.GetItem(context.Background(), "item")
		staleLookup <- err
	}()
	select {
	case <-staleLookupStarted:
	case <-time.After(time.Second):
		t.Fatal("second GetItem did not reach server")
	}
	if err := client.Move(context.Background(), "item", "new"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := client.ListDir(context.Background(), "new"); err != nil {
		t.Fatalf("ListDir new after Move: %v", err)
	}
	close(releaseStaleLookup)
	if err := <-staleLookup; err != nil {
		t.Fatalf("stale GetItem: %v", err)
	}
	if err := client.Rename(context.Background(), "item", "renamed"); err != nil {
		t.Fatalf("Rename after Move: %v", err)
	}
	if _, err := client.ListDir(context.Background(), "new"); err != nil {
		t.Fatalf("ListDir new after Rename: %v", err)
	}
	if _, err := client.ListDir(context.Background(), "unrelated"); err != nil {
		t.Fatalf("ListDir unrelated after Rename: %v", err)
	}
	mu.Lock()
	gotNew, gotUnrelated := listingCounts["new"], listingCounts["unrelated"]
	mu.Unlock()
	if gotNew != 3 || gotUnrelated != 1 {
		t.Fatalf("listing counts new:%d unrelated:%d, want 3/1", gotNew, gotUnrelated)
	}
}

func TestUploadDoesNotInvalidateDirectoryMetadata(t *testing.T) {
	var listRequests, uploadRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data") {
			uploadRequests.Add(1)
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		body := decodeGraphQLRequest(t, request)
		if body["operationName"] != "GetFileChildren5" {
			t.Errorf("unexpected operation %v", body["operationName"])
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		listRequests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": []any{}}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})
	if _, err := client.ListDir(context.Background(), "parent"); err != nil {
		t.Fatalf("initial ListDir: %v", err)
	}
	if err := client.Upload(context.Background(), AuthData{BaseURL: server.URL}, "file.bin", "checksum", 1, "application/octet-stream", strings.NewReader("x")); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := client.ListDir(context.Background(), "parent"); err != nil {
		t.Fatalf("ListDir after Upload: %v", err)
	}
	if listRequests.Load() != 1 || uploadRequests.Load() != 1 {
		t.Fatalf("listing/upload requests = %d/%d, want 1/1", listRequests.Load(), uploadRequests.Load())
	}
}

func waitForRequestStat(t *testing.T, client *Client, condition func(RequestStats) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition(client.RequestStats()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("request stats did not reach expected state: %+v", client.RequestStats())
}

func TestListDirDoesNotReturnCacheForCanceledContextOrCooldown(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": []any{}}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})
	if _, err := client.ListDir(context.Background(), "parent"); err != nil {
		t.Fatalf("initial ListDir: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListDir(canceled, "parent"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListDir with canceled context = %v, want context.Canceled", err)
	}
	client.rateLimitMu.Lock()
	client.rateLimitTill = time.Now().Add(time.Minute)
	client.rateLimitMu.Unlock()
	var rateLimitErr *RateLimitError
	if _, err := client.ListDir(context.Background(), "parent"); !errors.As(err, &rateLimitErr) {
		t.Fatalf("ListDir during cooldown = %v, want RateLimitError", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("cached calls made %d provider requests, want only initial request", got)
	}
}

func TestListDirCacheHasBoundedEntries(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": []any{}}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})
	for index := 0; index < metadataCacheMaxEntries+3; index++ {
		if _, err := client.ListDir(context.Background(), fmt.Sprintf("parent-%d", index)); err != nil {
			t.Fatalf("ListDir(%d): %v", index, err)
		}
	}
	client.metadataMu.Lock()
	got := len(client.metadataCache)
	client.metadataMu.Unlock()
	if got > metadataCacheMaxEntries {
		t.Fatalf("metadata cache entries = %d, want at most %d", got, metadataCacheMaxEntries)
	}
	if requests.Load() != metadataCacheMaxEntries+3 {
		t.Fatalf("provider requests = %d, want %d", requests.Load(), metadataCacheMaxEntries+3)
	}
}

func TestListDirMutationInvalidatesCacheOnSuccessAndError(t *testing.T) {
	mutations := []struct {
		name      string
		operation string
		invoke    func(*Client) error
	}{
		{
			name:      "rename",
			operation: "SetRenameFile",
			invoke: func(client *Client) error {
				return client.Rename(context.Background(), "item", "renamed")
			},
		},
		{
			name:      "register",
			operation: "SetUploadFile3",
			invoke: func(client *Client) error {
				_, err := client.Register(context.Background(), "parent", "file.bin", "checksum", 1, time.Now())
				return err
			},
		},
		{
			name:      "delete",
			operation: "SetDeleteFile5",
			invoke: func(client *Client) error {
				return client.Delete(context.Background(), "item", false)
			},
		},
	}
	for _, mutation := range mutations {
		for _, fail := range []bool{false, true} {
			name := "success"
			if fail {
				name = "error"
			}
			t.Run(mutation.name+"/"+name, func(t *testing.T) {
				var listRequests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					body := decodeGraphQLRequest(t, request)
					switch body["operationName"] {
					case "GetFileChildren5":
						listRequests.Add(1)
						writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": []any{}}}})
					case mutation.operation:
						if fail {
							writer.WriteHeader(http.StatusInternalServerError)
							return
						}
						field := map[string]string{"SetRenameFile": "setRenameFile", "SetUploadFile3": "setUploadFile3", "SetDeleteFile5": "setDeleteFile5"}[mutation.operation]
						writeJSON(t, writer, map[string]any{"data": map[string]any{field: true}})
					default:
						t.Errorf("unexpected operation %v", body["operationName"])
						writer.WriteHeader(http.StatusBadRequest)
					}
				}))
				defer server.Close()
				client := newTestClient(server, &testTokens{initial: "token"})
				if _, err := client.ListDir(context.Background(), "parent"); err != nil {
					t.Fatalf("prime ListDir cache: %v", err)
				}
				if got := mutation.invoke(client); (got != nil) != fail {
					t.Fatalf("mutation error = %v, want error=%t", got, fail)
				}
				if _, err := client.ListDir(context.Background(), "parent"); err != nil {
					t.Fatalf("ListDir after mutation: %v", err)
				}
				if _, err := client.ListDir(context.Background(), "parent"); err != nil {
					t.Fatalf("repeated ListDir during mutation visibility window: %v", err)
				}
				if got := listRequests.Load(); got != 2 {
					t.Fatalf("ListDir provider requests = %d, want 2 after mutation invalidation", got)
				}
			})
		}
	}
}

func TestListDirMutationInvalidatesInflightSnapshots(t *testing.T) {
	firstListStarted := make(chan struct{})
	finishFirstList := make(chan struct{})
	mutationStarted := make(chan struct{})
	finishMutation := make(chan struct{})
	var listRequests atomic.Int32
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		switch body["operationName"] {
		case "GetFileChildren5":
			page := listRequests.Add(1)
			if page == 1 {
				close(firstListStarted)
				<-finishFirstList
			}
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
				"Items": []map[string]string{{"ID": fmt.Sprintf("item-%d", page), "Name": "item"}},
			}}})
		case "SetRenameFile":
			once.Do(func() { close(mutationStarted) })
			<-finishMutation
			writeJSON(t, writer, map[string]any{"data": map[string]any{"setRenameFile": true}})
		default:
			t.Errorf("unexpected operation %v", body["operationName"])
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})

	firstResult := make(chan error, 1)
	go func() {
		_, err := client.ListDir(context.Background(), "parent")
		firstResult <- err
	}()
	select {
	case <-firstListStarted:
	case <-time.After(time.Second):
		t.Fatal("first ListDir did not reach server")
	}
	mutationResult := make(chan error, 1)
	go func() { mutationResult <- client.Rename(context.Background(), "item", "renamed") }()
	select {
	case <-mutationStarted:
	case <-time.After(time.Second):
		close(finishFirstList)
		t.Fatal("rename did not reach server")
	}

	if _, err := client.ListDir(context.Background(), "parent"); err != nil {
		close(finishMutation)
		close(finishFirstList)
		t.Fatalf("ListDir during mutation: %v", err)
	}
	close(finishMutation)
	if err := <-mutationResult; err != nil {
		close(finishFirstList)
		t.Fatalf("Rename: %v", err)
	}
	close(finishFirstList)
	if err := <-firstResult; err != nil {
		t.Fatalf("in-flight ListDir: %v", err)
	}
	items, err := client.ListDir(context.Background(), "parent")
	if err != nil {
		t.Fatalf("ListDir after in-flight response: %v", err)
	}
	if len(items) != 1 || items[0].ID != "item-3" || listRequests.Load() != 3 {
		t.Fatalf("post-mutation ListDir = %#v with %d requests, want item-3 and 3 requests", items, listRequests.Load())
	}
}

func TestGetWriteAuthReportsAlreadyExists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getBucketWriteAuth4": []any{map[string]any{"Error": "File already exists"}}}})
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: "token"}).GetWriteAuth(context.Background(), "parent", "file.txt", 12, "checksum")
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("GetWriteAuth error = %v, want ErrAlreadyExists", err)
	}
}

func TestGetWriteAuthBatchReturnsOrderedPerItemResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		if body["operationName"] != "GetBucketWriteAuth4" {
			t.Errorf("operationName = %v, want GetBucketWriteAuth4", body["operationName"])
		}
		variables := body["variables"].(map[string]any)
		if variables["ParentID"] != "parent" {
			t.Errorf("ParentID = %v, want parent", variables["ParentID"])
		}
		infos := variables["StorageUploadInfos"].([]any)
		if len(infos) != 2 || infos[0].(map[string]any)["FileName"] != "first.bin" || infos[1].(map[string]any)["FileName"] != "second.bin" {
			t.Errorf("StorageUploadInfos = %#v, want ordered two-item batch", infos)
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getBucketWriteAuth4": []any{
			map[string]any{"AuthData": map[string]any{"BaseURL": "https://storage.invalid", "KeyPrefix": "first/"}},
			map[string]any{"Error": "File already exists"},
		}}})
	}))
	defer server.Close()

	results, err := newTestClient(server, &testTokens{initial: "token"}).GetWriteAuthBatch(context.Background(), "parent", []UploadInfo{
		{Name: "first.bin", Size: 10, Checksum: "first-checksum"},
		{Name: "second.bin", Size: 20, Checksum: "second-checksum"},
	})
	if err != nil {
		t.Fatalf("GetWriteAuthBatch: %v", err)
	}
	if len(results) != 2 || results[0].AuthData.KeyPrefix != "first/" || results[0].Err != nil {
		t.Fatalf("first result = %+v; want first authorization", results[0])
	}
	if !errors.Is(results[1].Err, ErrAlreadyExists) {
		t.Fatalf("second result error = %v, want ErrAlreadyExists", results[1].Err)
	}
}

func TestGetWriteAuthBatchRequiresOneResultPerInput(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getBucketWriteAuth4": []any{
			map[string]any{"AuthData": map[string]any{"BaseURL": "https://storage.invalid"}},
		}}})
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: "token"}).GetWriteAuthBatch(context.Background(), "parent", []UploadInfo{
		{Name: "first.bin", Size: 10, Checksum: "first"},
		{Name: "second.bin", Size: 20, Checksum: "second"},
	})
	if err == nil || !strings.Contains(err.Error(), "returned 1 authorizations for 2 uploads") {
		t.Fatalf("GetWriteAuthBatch error = %v, want response-cardinality error", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want one", requests.Load())
	}
}

func TestGetWriteAuthBatchRejectsOversizedBatchWithoutRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getBucketWriteAuth4": []any{}}})
	}))
	defer server.Close()

	infos := make([]UploadInfo, maxWriteAuthBatch+1)
	if _, err := newTestClient(server, &testTokens{initial: "token"}).GetWriteAuthBatch(context.Background(), "parent", infos); err == nil {
		t.Fatal("GetWriteAuthBatch unexpectedly accepted an oversized batch")
	}
	if requests.Load() != 0 {
		t.Fatalf("oversized batch sent %d requests, want zero", requests.Load())
	}
}

func TestRegisterBatchSendsOneMutationWithAllRecords(t *testing.T) {
	created := time.UnixMilli(1700000000123)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		body := decodeGraphQLRequest(t, request)
		if body["operationName"] != "SetUploadFile3" {
			t.Errorf("operationName = %v, want SetUploadFile3", body["operationName"])
		}
		variables := body["variables"].(map[string]any)
		infos := variables["FileInfos"].([]any)
		if len(infos) != 2 {
			t.Errorf("FileInfos length = %d, want 2", len(infos))
		}
		first := infos[0].(map[string]any)
		second := infos[1].(map[string]any)
		if first["ParentID"] != "parent-a" || first["Name"] != "a.bin" || first["Size"] != "10" || first["Checksum"] != "a-checksum" || first["CreationTime"] != "1700000000123" {
			t.Errorf("first FileInfo = %#v", first)
		}
		if second["ParentID"] != "parent-b" || second["Name"] != "b.bin" || second["Size"] != "20" || second["Checksum"] != "b-checksum" {
			t.Errorf("second FileInfo = %#v", second)
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"setUploadFile3": true}})
	}))
	defer server.Close()

	result, err := newTestClient(server, &testTokens{initial: "token"}).RegisterBatch(context.Background(), []RegistrationInfo{
		{ParentID: "parent-a", Name: "a.bin", Size: 10, Checksum: "a-checksum", Created: created},
		{ParentID: "parent-b", Name: "b.bin", Size: 20, Checksum: "b-checksum", Created: created},
	})
	if err != nil || result != "" {
		t.Fatalf("RegisterBatch = %q, %v; want empty success acknowledgement", result, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("RegisterBatch sent %d requests, want one", requests.Load())
	}
}

func TestRegisterBatchRejectsOversizedBatchWithoutRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"setUploadFile3": true}})
	}))
	defer server.Close()

	infos := make([]RegistrationInfo, maxRegisterBatch+1)
	if _, err := newTestClient(server, &testTokens{initial: "token"}).RegisterBatch(context.Background(), infos); err == nil {
		t.Fatal("RegisterBatch unexpectedly accepted an oversized batch")
	}
	if requests.Load() != 0 {
		t.Fatalf("oversized batch sent %d requests, want zero", requests.Load())
	}
}

func TestRequestStatsCountSafeOperationNamesAndClassifyWriteAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		switch body["operationName"] {
		case "GetOverlay4":
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getOverlay4": map[string]any{"ID": "17", "Name": "file", "ParentID": "parent", "Category": 1, "Size": "1", "URL": "https://storage.invalid/object"}}})
		case "GetBucketWriteAuth4":
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getBucketWriteAuth4": []any{map[string]any{"AuthData": map[string]any{"BaseURL": "https://storage.invalid"}}}}})
		default:
			t.Errorf("unexpected operation %v", body["operationName"])
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})
	if _, err := client.GetItem(context.Background(), "17"); err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if _, err := client.GetWriteAuth(context.Background(), "parent", "file", 1, "checksum"); err != nil {
		t.Fatalf("GetWriteAuth: %v", err)
	}
	stats := client.RequestStats()
	if stats.HTTPRequests != 2 || stats.ReadHTTPRequests != 1 || stats.WriteHTTPRequests != 1 {
		t.Fatalf("HTTP request totals = %+v, want 2 total (1 read, 1 write)", stats)
	}
	if stats.RequestsByOperation["GetOverlay4"] != 1 || stats.RequestsByOperation["GetBucketWriteAuth4"] != 1 {
		t.Fatalf("operation totals = %#v", stats.RequestsByOperation)
	}
	stats.RequestsByOperation["GetOverlay4"] = 100
	if client.RequestStats().RequestsByOperation["GetOverlay4"] != 1 {
		t.Fatal("mutating stats snapshot changed client counters")
	}
}

func TestGetWriteAuthPassesStorageChecksum(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		if body == nil {
			return
		}
		variables := body["variables"].(map[string]any)
		infos := variables["StorageUploadInfos"].([]any)
		info := infos[0].(map[string]any)
		if info["FileName"] != "file.txt" || info["Checksum"] != "provider-checksum" || info["Size"] != "129" {
			t.Errorf("StorageUploadInfos = %#v", info)
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getBucketWriteAuth4": []any{map[string]any{
			"AuthData": map[string]any{
				"PolicyBase64": "policy", "Signature": "signature", "BaseURL": "https://storage.invalid/",
				"KeyPrefix": "prefix/", "AccessKey": map[string]string{"Key": "AWSAccessKeyId", "Value": "access"},
				"AdditionalBody": []map[string]string{{"Key": "Cache-Control", "Value": "no-cache"}},
			},
		}}}})
	}))
	defer server.Close()

	got, err := newTestClient(server, &testTokens{initial: "token"}).GetWriteAuth(context.Background(), "parent", "file.txt", 129, "provider-checksum")
	if err != nil {
		t.Fatalf("GetWriteAuth: %v", err)
	}
	if got.PolicyBase64 != "policy" || got.AccessKey == nil || got.AccessKey.Key != "AWSAccessKeyId" || len(got.AdditionalBody) != 1 {
		t.Fatalf("unexpected auth data: %+v", got)
	}
}

func TestGraphQLErrorDoesNotExposeEchoedToken(t *testing.T) {
	const secretToken = "token-that-must-not-appear"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, map[string]any{"errors": []any{map[string]any{
			"message": "upstream echoed " + secretToken,
		}}})
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: secretToken}).GetItem(context.Background(), "17")
	if err == nil {
		t.Fatal("GetItem unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("API error disclosed the access token: %v", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || len(apiErr.graphQLErrors) != 1 || apiErr.graphQLErrors[0].message != "GraphQL request failed" {
		t.Fatalf("unexpected sanitized API error: %#v", apiErr)
	}
}

func TestGraphQLErrorShowsSafeClassification(t *testing.T) {
	for _, test := range []struct {
		typ, want string
	}{
		{"ValidationException", "ValidationException"},
		{"NotFoundException", "NotFoundException"},
		{"ThrottlingException", "ThrottlingException"},
		{"ServiceUnavailableException", "ServiceUnavailableException"},
		{"InternalFailure", "InternalFailure"},
		{"private-provider-type", "GraphQLException"},
	} {
		t.Run(test.typ, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writeJSON(t, writer, map[string]any{"errors": []any{map[string]any{
					"message":   "provider echoed private-token and private-path",
					"errorType": test.typ,
				}}})
			}))
			defer server.Close()
			_, err := newTestClient(server, &testTokens{initial: "private-token"}).GetItem(context.Background(), "17")
			require.ErrorContains(t, err, "GraphQL request failed ("+test.want+")")
			require.NotContains(t, err.Error(), "private-")
		})
	}
}

func TestHTTPErrorDoesNotExposeBodyAndCarriesBoundedRetryAfter(t *testing.T) {
	const secretToken = "echoed-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "900")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(writer, "upstream response included "+secretToken+" and a signed URL")
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: "token"}).ListDir(context.Background(), "parent")
	if err == nil {
		t.Fatal("ListDir unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "signed URL") {
		t.Fatalf("API error disclosed response body: %v", err)
	}
	if HTTPStatus(err) != http.StatusServiceUnavailable || !Retryable(err) {
		t.Fatalf("status/retryable = %d/%t", HTTPStatus(err), Retryable(err))
	}
	delay, ok := pacer.IsRetryAfter(err)
	if !ok || delay != maxRetryAfter {
		t.Fatalf("Retry-After = %v, %t; want bounded delay %v", delay, ok, maxRetryAfter)
	}
}

func TestHTTP503HonorsShortRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "2")
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: "token"}).ListDir(context.Background(), "parent")
	if HTTPStatus(err) != http.StatusServiceUnavailable || !Retryable(err) {
		t.Fatalf("status/retryable = %d/%t, want 503/true", HTTPStatus(err), Retryable(err))
	}
	delay, ok := pacer.IsRetryAfter(err)
	if !ok || delay != 2*time.Second {
		t.Fatalf("Retry-After = %v, %t; want 2s", delay, ok)
	}
}

func TestHTTP429HonorsLongRetryAfterAndBlocksBeforeToken(t *testing.T) {
	tokens := &testTokens{initial: "token"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Retry-After", "65043")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := newTestClient(server, tokens)
	before := time.Now()
	_, err := client.ListDir(context.Background(), "parent")
	if err == nil {
		t.Fatal("ListDir unexpectedly succeeded")
	}
	var rateErr *RateLimitError
	if !errors.As(err, &rateErr) {
		t.Fatalf("error = %T %v, want *RateLimitError", err, err)
	}
	if got := rateErr.Until.Sub(before); got < 65042*time.Second || got > 65044*time.Second {
		t.Fatalf("rate limit until = %s, duration from request start %s; want about 65043s", rateErr.Until, got)
	}
	if HTTPStatus(err) != http.StatusTooManyRequests || Retryable(err) {
		t.Fatalf("status/retryable = %d/%t, want 429/false", HTTPStatus(err), Retryable(err))
	}
	if _, ok := pacer.IsRetryAfter(err); ok {
		t.Fatal("429 returned a shared-pacer retry delay")
	}
	if requests.Load() != 1 || tokens.calls.Load() != 1 {
		t.Fatalf("requests/token calls = %d/%d, want 1/1", requests.Load(), tokens.calls.Load())
	}

	firstUntil := rateErr.Until
	const concurrentCalls = 16
	var wg sync.WaitGroup
	errs := make(chan error, concurrentCalls)
	for range concurrentCalls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.ListDir(context.Background(), "parent")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		var blockedErr *RateLimitError
		if !errors.As(err, &blockedErr) || !blockedErr.Until.Equal(firstUntil) {
			t.Errorf("concurrent call error = %T %v, want rate limit until %s", err, err, firstUntil)
		}
	}
	if requests.Load() != 1 || tokens.calls.Load() != 1 {
		t.Fatalf("concurrent blocked calls made provider/auth activity: requests/token calls = %d/%d", requests.Load(), tokens.calls.Load())
	}
}

func TestRateLimitCooldownPersistsAndReloads(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rateLimitFile := filepath.Join(privateDir, "auth.json.rate-limit.json")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Retry-After", "65043")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	firstTokens := &testTokens{initial: "token"}
	first := newTestClient(server, firstTokens)
	if err := first.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("SetRateLimitFile: %v", err)
	}
	_, err := first.ListDir(context.Background(), "parent")
	var rateErr *RateLimitError
	if !errors.As(err, &rateErr) {
		t.Fatalf("ListDir error = %T %v, want *RateLimitError", err, err)
	}
	data, err := os.ReadFile(rateLimitFile)
	if err != nil {
		t.Fatalf("read cooldown sidecar: %v", err)
	}
	var saved struct {
		Version   int   `json:"version"`
		UntilUnix int64 `json:"until_unix"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("decode cooldown sidecar: %v", err)
	}
	if saved.Version != 1 || saved.UntilUnix < rateErr.Until.Unix() {
		t.Fatalf("saved cooldown = %+v, error deadline = %s", saved, rateErr.Until)
	}
	info, err := os.Stat(rateLimitFile)
	if err != nil {
		t.Fatalf("stat cooldown sidecar: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar mode = %v, want 0600", info.Mode().Perm())
	}

	secondTokens := &testTokens{initial: "token"}
	second := newTestClient(server, secondTokens)
	if err := second.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("reload SetRateLimitFile: %v", err)
	}
	_, err = second.ListDir(context.Background(), "parent")
	if !errors.As(err, &rateErr) || rateErr.Until.Unix() != saved.UntilUnix {
		t.Fatalf("reloaded call error = %T %v; want persisted deadline %d", err, err, saved.UntilUnix)
	}
	if requests.Load() != 1 || secondTokens.calls.Load() != 0 {
		t.Fatalf("reloaded cooldown made provider/auth activity: requests/token calls = %d/%d", requests.Load(), secondTokens.calls.Load())
	}
}

func TestSharedRateLimitSidecarBlocksInitializedClient(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rateLimitFile := filepath.Join(privateDir, "auth.json.rate-limit.json")
	var firstRequests atomic.Int32
	firstServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		firstRequests.Add(1)
		writer.Header().Set("Retry-After", "65043")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer firstServer.Close()

	firstTokens := &testTokens{initial: "token"}
	first := newTestClient(firstServer, firstTokens)
	if err := first.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("first SetRateLimitFile: %v", err)
	}
	secondTokens := &testTokens{initial: "token"}
	var secondRequests atomic.Int32
	secondServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		secondRequests.Add(1)
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"data":{"getFileChildren5":{"Items":[],"NextToken":""}}}`)
	}))
	defer secondServer.Close()
	second := newTestClient(secondServer, secondTokens)
	if err := second.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("second SetRateLimitFile: %v", err)
	}

	_, err := first.ListDir(context.Background(), "parent")
	var firstRateErr *RateLimitError
	if !errors.As(err, &firstRateErr) {
		t.Fatalf("first ListDir error = %T %v, want *RateLimitError", err, err)
	}
	_, err = second.ListDir(context.Background(), "parent")
	var secondRateErr *RateLimitError
	if !errors.As(err, &secondRateErr) || secondRateErr.Until.Before(firstRateErr.Until) {
		t.Fatalf("second ListDir error = %T %v; want shared deadline %s", err, err, firstRateErr.Until)
	}
	if firstRequests.Load() != 1 || secondRequests.Load() != 0 || firstTokens.calls.Load() != 1 || secondTokens.calls.Load() != 0 {
		t.Fatalf("requests/token calls = %d/%d / %d/%d; second client must fail before auth or HTTP", firstRequests.Load(), firstTokens.calls.Load(), secondRequests.Load(), secondTokens.calls.Load())
	}
}

func TestGraphQLAuthRefreshRechecksSharedRateLimit(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rateLimitFile := filepath.Join(privateDir, "auth.json.rate-limit.json")
	cooldownServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "65043")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer cooldownServer.Close()
	cooldownClient := newTestClient(cooldownServer, &testTokens{initial: "token"})
	if err := cooldownClient.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("cooldown client SetRateLimitFile: %v", err)
	}

	requestStarted := make(chan struct{})
	allowUnauthorized := make(chan struct{})
	var releaseOnce sync.Once
	releaseResponse := func() { releaseOnce.Do(func() { close(allowUnauthorized) }) }
	var waitingRequests atomic.Int32
	waitingServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		waitingRequests.Add(1)
		close(requestStarted)
		<-allowUnauthorized
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer waitingServer.Close()
	defer releaseResponse()
	waitingTokens := &testTokens{initial: "expired", refreshed: "new-token"}
	waitingClient := newTestClient(waitingServer, waitingTokens)
	if err := waitingClient.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("waiting client SetRateLimitFile: %v", err)
	}

	waitingErr := make(chan error, 1)
	go func() {
		_, err := waitingClient.ListDir(context.Background(), "parent")
		waitingErr <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("waiting client did not start its request")
	}
	if _, err := cooldownClient.ListDir(context.Background(), "parent"); err == nil {
		t.Fatal("cooldown client unexpectedly succeeded")
	}
	releaseResponse()
	select {
	case err := <-waitingErr:
		var rateErr *RateLimitError
		if !errors.As(err, &rateErr) {
			t.Fatalf("waiting ListDir error = %T %v, want shared RateLimitError", err, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiting client did not stop after its unauthorized response")
	}
	if waitingRequests.Load() != 1 || waitingTokens.calls.Load() != 1 || waitingTokens.refreshes.Load() != 0 {
		t.Fatalf("waiting client activity = requests %d, Token calls %d, refreshes %d; want one initial request and no refresh", waitingRequests.Load(), waitingTokens.calls.Load(), waitingTokens.refreshes.Load())
	}
}

func TestConcurrentClientsCannotShortenSharedRateLimit(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rateLimitFile := filepath.Join(privateDir, "auth.json.rate-limit.json")

	firstServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "65043")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer firstServer.Close()
	first := newTestClient(firstServer, &testTokens{initial: "token"})
	if err := first.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("first SetRateLimitFile: %v", err)
	}

	requestStarted := make(chan struct{})
	allowResponse := make(chan struct{})
	var releaseOnce sync.Once
	releaseResponse := func() { releaseOnce.Do(func() { close(allowResponse) }) }
	secondServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-allowResponse
		writer.Header().Set("Retry-After", "60")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer secondServer.Close()
	defer releaseResponse()
	second := newTestClient(secondServer, &testTokens{initial: "token"})
	if err := second.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("second SetRateLimitFile: %v", err)
	}

	secondErr := make(chan error, 1)
	go func() {
		_, err := second.ListDir(context.Background(), "parent")
		secondErr <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("second client did not start its request")
	}
	_, err := first.ListDir(context.Background(), "parent")
	var firstRateErr *RateLimitError
	if !errors.As(err, &firstRateErr) {
		t.Fatalf("first ListDir error = %T %v, want *RateLimitError", err, err)
	}
	releaseResponse()
	select {
	case err := <-secondErr:
		var secondRateErr *RateLimitError
		if !errors.As(err, &secondRateErr) {
			t.Fatalf("second ListDir error = %T %v, want *RateLimitError", err, err)
		}
		if secondRateErr.Until.Before(firstRateErr.Until) {
			t.Fatalf("second deadline = %s, want shared maximum %s", secondRateErr.Until, firstRateErr.Until)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second client did not finish its request")
	}

	data, err := os.ReadFile(rateLimitFile)
	if err != nil {
		t.Fatalf("read shared cooldown sidecar: %v", err)
	}
	savedUntil, err := decodeRateLimitFile(data)
	if err != nil {
		t.Fatalf("decode shared cooldown sidecar: %v", err)
	}
	if savedUntil.Before(firstRateErr.Until) {
		t.Fatalf("persisted deadline = %s, want at least %s", savedUntil, firstRateErr.Until)
	}
}

func TestRateLimitSidecarLockIsPrivateAndRejectsSymlink(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "netbsd", "openbsd", "dragonfly", "solaris":
	default:
		t.Skip("the platform lock implementation is not available on this OS")
	}
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rateLimitFile := filepath.Join(privateDir, "auth.json.rate-limit.json")
	if err := New(nil, nil, "", "").SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("SetRateLimitFile: %v", err)
	}
	lockInfo, err := os.Lstat(rateLimitFile + ".lock")
	if err != nil {
		t.Fatalf("stat sidecar lock: %v", err)
	}
	if !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar lock mode = %v, want regular 0600", lockInfo.Mode())
	}

	otherPath := filepath.Join(privateDir, "other.rate-limit.json")
	target := filepath.Join(privateDir, "target")
	if err := auth.WritePrivateFile(target, []byte("private")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, otherPath+".lock"); err != nil {
		t.Fatal(err)
	}
	if err := New(nil, nil, "", "").SetRateLimitFile(otherPath); err == nil {
		t.Fatal("SetRateLimitFile unexpectedly accepted a symlink lock")
	}
}

func TestRateLimitPersistenceFailureKeepsLongestDeadline(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rateLimitFile := filepath.Join(privateDir, "auth.json.rate-limit.json")
	client := New(nil, nil, "", "")
	longUntil := time.Now().Add(65043 * time.Second)
	client.rateLimitMu.Lock()
	client.rateLimitTill = longUntil
	client.rateLimitMu.Unlock()
	if err := client.SetRateLimitFile(rateLimitFile); err != nil {
		t.Fatalf("SetRateLimitFile: %v", err)
	}
	if err := os.Chmod(rateLimitFile+".lock", 0o644); err != nil {
		t.Fatal(err)
	}
	response := &http.Response{Header: make(http.Header)}
	response.Header.Set("Retry-After", "60")

	err := client.rateLimited("GetFileChildren5", response, errors.New("provider rejected request"))
	var rateErr *RateLimitError
	if !errors.As(err, &rateErr) {
		t.Fatalf("error = %T %v, want *RateLimitError", err, err)
	}
	if !rateErr.Until.Equal(longUntil) || !rateErr.RetryAfter().Equal(longUntil) {
		t.Fatalf("returned deadline = %s, RetryAfter=%s; want existing maximum %s", rateErr.Until, rateErr.RetryAfter(), longUntil)
	}
	if rateErr.persistErr == nil || !strings.Contains(err.Error(), "local cooldown could not be persisted") {
		t.Fatalf("persistence error not surfaced safely: %v", err)
	}
	if HTTPStatus(err) != http.StatusTooManyRequests || Retryable(err) {
		t.Fatalf("status/retryable = %d/%t, want 429/false", HTTPStatus(err), Retryable(err))
	}
}

func TestRateLimitCooldownWithoutRetryAfterUsesDefault(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := newTestClient(server, &testTokens{initial: "token"})
	before := time.Now()
	_, err := client.ListDir(context.Background(), "parent")
	var rateErr *RateLimitError
	if !errors.As(err, &rateErr) {
		t.Fatalf("ListDir error = %T %v, want *RateLimitError", err, err)
	}
	if got := rateErr.Until.Sub(before); got < defaultRateLimitCooldown-time.Second || got > defaultRateLimitCooldown+time.Second {
		t.Fatalf("default cooldown = %s, want about %s", got, defaultRateLimitCooldown)
	}
	_, _ = client.ListDir(context.Background(), "parent")
	if requests.Load() != 1 {
		t.Fatalf("repeated request count = %d, want 1", requests.Load())
	}
}

func TestRateLimitCooldownCancellationSkipsAuth(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Retry-After", "60")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	tokens := &testTokens{initial: "token"}
	client := newTestClient(server, tokens)
	_, _ = client.ListDir(context.Background(), "parent")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.ListDir(ctx, "parent")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call error = %v, want context.Canceled", err)
	}
	if requests.Load() != 1 || tokens.calls.Load() != 1 {
		t.Fatalf("canceled call made provider/auth activity: requests/token calls = %d/%d", requests.Load(), tokens.calls.Load())
	}
}

func TestSetRateLimitFileRejectsInvalidPrivateFiles(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Run("unsupported version", func(t *testing.T) {
		file := filepath.Join(privateDir, "invalid.json")
		if err := os.WriteFile(file, []byte(`{"version":2,"until_unix":123}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := New(nil, nil, "", "").SetRateLimitFile(file); err == nil {
			t.Fatal("SetRateLimitFile unexpectedly accepted invalid state")
		}
	})
	t.Run("unsafe permissions", func(t *testing.T) {
		file := filepath.Join(privateDir, "public.json")
		if err := os.WriteFile(file, []byte(`{"version":1,"until_unix":123}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := New(nil, nil, "", "").SetRateLimitFile(file); err == nil {
			t.Fatal("SetRateLimitFile unexpectedly accepted public state")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := filepath.Join(privateDir, "target.json")
		if err := os.WriteFile(target, []byte(`{"version":1,"until_unix":123}`), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(privateDir, "link.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := New(nil, nil, "", "").SetRateLimitFile(link); err == nil {
			t.Fatal("SetRateLimitFile unexpectedly followed a symlink")
		}
	})
}

func TestRetryAfterUntilParsesDatesAndSaturatesOverflow(t *testing.T) {
	now := time.Date(2026, time.October, 13, 20, 2, 13, 0, time.UTC)
	want := now.Add(65043 * time.Second)
	got, ok := retryAfterUntil(want.Format(http.TimeFormat), now)
	if !ok || !got.Equal(want) {
		t.Fatalf("date Retry-After = %s, %t; want %s", got, ok, want)
	}

	got, ok = retryAfterUntil(strings.Repeat("9", 256), now)
	if !ok || !got.After(time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)) {
		t.Fatalf("overflow Retry-After = %s, %t; want saturated future deadline", got, ok)
	}
}

func TestGCSUpload429PersistsCooldown(t *testing.T) {
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Retry-After", "60")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	tokens := &testTokens{initial: "token"}
	client := newTestClient(server, tokens)
	if err := client.SetRateLimitFile(filepath.Join(privateDir, "auth.json.rate-limit.json")); err != nil {
		t.Fatal(err)
	}
	err := client.Upload(context.Background(), AuthData{BaseURL: server.URL}, "file.txt", "checksum", 4, "text/plain", strings.NewReader("data"))
	var rateErr *RateLimitError
	if !errors.As(err, &rateErr) || HTTPStatus(err) != http.StatusTooManyRequests {
		t.Fatalf("Upload error = %T %v, want rate limit with HTTP 429", err, err)
	}
	_, _ = client.ListDir(context.Background(), "parent")
	if requests.Load() != 1 || tokens.calls.Load() != 0 {
		t.Fatalf("upload cooldown made provider/auth activity: requests/token calls = %d/%d", requests.Load(), tokens.calls.Load())
	}
}

func TestMutation429DoesNotReplay(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Retry-After", "60")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	_, err := newTestClient(server, &testTokens{initial: "token"}).Register(context.Background(), "parent", "file.txt", "checksum", 4, time.Now())
	var rateErr *RateLimitError
	if !errors.As(err, &rateErr) || Retryable(err) || requests.Load() != 1 {
		t.Fatalf("Register error = %T %v, retryable=%t, requests=%d; want one nonretryable 429", err, err, Retryable(err), requests.Load())
	}
}

func TestRefresh403ProducesSanitizedLoginDiagnostic(t *testing.T) {
	const refreshSecret = "private-refresh-token-must-not-leak"
	expiredToken := expiredTestJWT(t)
	state, err := json.Marshal(map[string]string{"token": expiredToken, "refresh_token": refreshSecret})
	if err != nil {
		t.Fatal(err)
	}
	privateDir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(privateDir, "auth.json")
	if err := auth.WritePrivateFile(authFile, state); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/access-token/v2" {
			t.Errorf("request path = %q, want /access-token/v2", request.URL.Path)
		}
		writer.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(writer, "provider echoed "+refreshSecret)
	}))
	defer server.Close()

	tokens, err := auth.New(server.Client(), authFile, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = newTestClient(server, tokens).ListDir(context.Background(), "parent")
	if err == nil {
		t.Fatal("ListDir unexpectedly succeeded")
	}
	message := err.Error()
	if !strings.Contains(message, "cannot obtain access token") || !strings.Contains(message, "degoo login required") || !strings.Contains(message, "auth HTTP 403") {
		t.Fatalf("error = %q, want actionable sanitized auth classification and status", message)
	}
	if strings.Contains(message, refreshSecret) || strings.Contains(message, "provider echoed") {
		t.Fatalf("error disclosed refresh credentials or response body: %q", message)
	}
	if requests.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", requests.Load())
	}
}

func TestAPIErrorDoesNotExposeArbitraryWrappedCause(t *testing.T) {
	const secret = "arbitrary-token-response-secret"
	err := &Error{operation: "GetFileChildren5", message: "cannot obtain access token", err: fmt.Errorf("provider detail %s", secret)}
	message := err.Error()
	if strings.Contains(message, secret) || strings.Contains(message, "provider detail") {
		t.Fatalf("API error disclosed arbitrary wrapped cause: %q", message)
	}
}

func expiredTestJWT(t *testing.T) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]int64{"exp": time.Now().Add(-time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}

func TestMutationDoesNotReturnAutomaticRetryAfter(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Retry-After", "30")
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: "token"}).Register(context.Background(), "parent", "file.txt", "checksum", 1, time.Now())
	if err == nil {
		t.Fatal("Register unexpectedly succeeded")
	}
	if _, ok := pacer.IsRetryAfter(err); ok || requests.Load() != 1 {
		t.Fatalf("Register returned retry-after=%t after %d requests", ok, requests.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestDownloadErrorDoesNotExposeSignedURL(t *testing.T) {
	const secret = "signed-url-secret"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial failed for %s", request.URL.String())
	})}
	api := New(client, &testTokens{initial: "unused"}, "https://graphql.invalid", "key")
	_, err := api.Download(context.Background(), "https://cdn.invalid/file?signature="+secret, "")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("Download error = %v; want a sanitized error", err)
	}
}

func TestRegisterPassesChecksumAndMillisecondCreationTime(t *testing.T) {
	created := time.UnixMilli(1700000000123)
	const checksum = "ChQVgjMd4f9UAnRnJNB8-dCTOpsPLBAA"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		variables := body["variables"].(map[string]any)
		fileInfos := variables["FileInfos"].([]any)
		fileInfo := fileInfos[0].(map[string]any)
		if fileInfo["ParentID"] != "parent" || fileInfo["Name"] != "file.bin" || fileInfo["Size"] != "129" {
			t.Errorf("unexpected FileInfo fields: %#v", fileInfo)
		}
		if fileInfo["Checksum"] != checksum || fileInfo["CreationTime"] != "1700000000123" {
			t.Errorf("Checksum/CreationTime = %v/%v", fileInfo["Checksum"], fileInfo["CreationTime"])
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"setUploadFile3": "OK"}})
	}))
	defer server.Close()

	result, err := newTestClient(server, &testTokens{initial: "token"}).Register(context.Background(), "parent", "file.bin", checksum, 129, created)
	if err != nil || result != "OK" {
		t.Fatalf("Register = %q, %v; want OK, nil", result, err)
	}
}

func TestRegisterTransportFailureIsNotReplayed(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		conn, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack response: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: "token"}).Register(context.Background(), "parent", "file.bin", "checksum", 1, time.Now())
	if err == nil {
		t.Fatal("Register unexpectedly succeeded")
	}
	if requests.Load() != 1 {
		t.Fatalf("Register sent %d requests after an ambiguous failure, want 1", requests.Load())
	}
}

func TestDownloadValidatesRangeResponse(t *testing.T) {
	tests := []struct {
		name         string
		contentRange string
		wantErr      bool
	}{
		{name: "valid", contentRange: "bytes 3-5/10"},
		{name: "wrong range", contentRange: "bytes 2-4/10", wantErr: true},
		{name: "missing range", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if got := request.Header.Get("Range"); got != "bytes=3-5" {
					t.Errorf("Range = %q, want bytes=3-5", got)
				}
				writer.Header().Set("Content-Range", test.contentRange)
				writer.Header().Set("Content-Length", "3")
				writer.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(writer, "xyz")
			}))
			defer server.Close()

			response, err := New(server.Client(), &testTokens{initial: "unused"}, server.URL, "key").Download(context.Background(), server.URL, "bytes=3-5")
			if test.wantErr {
				if err == nil || response != nil {
					t.Fatalf("Download = %v, %v; want nil response and error", response, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			defer func() { require.NoError(t, response.Body.Close()) }()
			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != "xyz" {
				t.Fatalf("download body = %q, %v", body, err)
			}
		})
	}
}

func TestDownloadRequiresFullResponseForUnrangedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusPartialContent)
	}))
	defer server.Close()

	response, err := New(server.Client(), &testTokens{initial: "unused"}, server.URL, "key").Download(context.Background(), server.URL, "")
	if err == nil || response != nil {
		t.Fatalf("Download = %v, %v; want nil response and error", response, err)
	}
}

func TestUploadUsesChecksumKeyAndFormFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1024); err != nil {
			t.Errorf("parse upload form: %v", err)
			return
		}
		wantFields := map[string]string{
			"AWSAccessKeyId": "access", "policy": "policy", "signature": "signature",
			"key": "prefix/txt/provider-checksum.txt", "Content-Type": "text/plain", "acl": "private",
			"Cache-Control": "no-cache",
		}
		for key, want := range wantFields {
			if got := request.FormValue(key); got != want {
				t.Errorf("form %s = %q, want %q", key, got, want)
			}
		}
		file, header, err := request.FormFile("file")
		if err != nil {
			t.Errorf("get uploaded file: %v", err)
			return
		}
		defer func() { require.NoError(t, file.Close()) }()
		content, err := io.ReadAll(file)
		if err != nil {
			t.Errorf("read uploaded file: %v", err)
		}
		if header.Filename != "report.TXT" || string(content) != "payload" {
			t.Errorf("uploaded file = %q, %q", header.Filename, content)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	auth := AuthData{
		PolicyBase64: "policy", Signature: "signature", BaseURL: server.URL, KeyPrefix: "prefix/",
		AccessKey: &KeyValue{Key: "AWSAccessKeyId", Value: "access"}, ACL: "private",
		AdditionalBody: []KeyValue{{Key: "Cache-Control", Value: "no-cache"}},
	}
	err := New(server.Client(), &testTokens{initial: "unused"}, server.URL, "key").Upload(
		context.Background(), auth, "report.TXT", "provider-checksum", 7, "text/plain", strings.NewReader("payload"),
	)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
}

func TestUploadRejectsNonHTTPSOrSignedBaseURL(t *testing.T) {
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected network request")
	})}
	api := New(client, &testTokens{initial: "unused"}, "https://graphql.invalid", "key")
	for _, baseURL := range []string{
		"http://storage.example/upload",
		"https://storage.example/upload?signature=private-value",
		"https://user:password@storage.example/upload",
	} {
		auth := AuthData{BaseURL: baseURL}
		err := api.Upload(context.Background(), auth, "file.txt", "checksum", 0, "text/plain", strings.NewReader(""))
		if err == nil || strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "password") {
			t.Errorf("Upload(%q) error = %v; want a sanitized validation error", baseURL, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid upload endpoints caused %d requests", requests.Load())
	}
}

func TestUploadErrorDoesNotExposeBaseURL(t *testing.T) {
	const secret = "signed-upload-path"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, request.URL.Path+"?signature="+secret)
	}))
	defer server.Close()
	api := New(server.Client(), &testTokens{initial: "unused"}, server.URL, "key")
	err := api.Upload(context.Background(), AuthData{BaseURL: server.URL + "/" + secret}, "file.txt", "checksum", 0, "text/plain", strings.NewReader(""))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("Upload error = %v; want a sanitized error", err)
	}
}

func TestChecksumRepresentationAndReadLimit(t *testing.T) {
	reader := bytes.NewReader(append(bytes.Repeat([]byte("x"), 1024*1024), []byte("tail-data")...))
	got, err := Checksum(reader)
	if err != nil {
		t.Fatalf("Checksum: %v", err)
	}
	if got != "ChQMncnku4VgmjrTNezGrRW9xWAEJhAA" {
		t.Fatalf("Checksum = %q, want provider protobuf/base64url value", got)
	}
	if reader.Len() != len("tail-data") {
		t.Fatalf("Checksum consumed %d excess bytes", len("tail-data")-reader.Len())
	}
}

func TestItemUnmarshalJSONRecycleFlag(t *testing.T) {
	data := []byte(`{"ID":"17","Name":"example.txt","ParentID":"3","Category":6,"Size":"42","IsInRecycleBin":true}`)
	var item Item
	if err := json.Unmarshal(data, &item); err != nil {
		t.Fatalf("unmarshal Item: %v", err)
	}
	if !item.IsInRecycleBin {
		t.Fatal("IsInRecycleBin was not decoded")
	}
}

func TestListTrashPaginatesAndHonorsLimit(t *testing.T) {
	tokens := &testTokens{initial: "token"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		if body == nil {
			return
		}
		variables := body["variables"].(map[string]any)
		if variables["Order"] != float64(3) {
			t.Errorf("Order = %v, want 3", variables["Order"])
		}
		page := requests.Add(1)
		if page == 1 {
			if variables["Limit"] != float64(2) {
				t.Errorf("first-page Limit = %v, want 2", variables["Limit"])
			}
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getDeletedFiles": map[string]any{
				"Items":     []map[string]any{{"ID": "1", "Name": "first", "ParentID": "parent", "Category": 6, "Size": "10", "IsInRecycleBin": true}},
				"NextToken": "opaque-cursor",
			}}})
			return
		}
		if variables["NextToken"] != "opaque-cursor" {
			t.Errorf("NextToken = %v, want opaque-cursor", variables["NextToken"])
		}
		if variables["Limit"] != float64(1) {
			t.Errorf("second-page Limit = %v, want 1", variables["Limit"])
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getDeletedFiles": map[string]any{
			"Items": []map[string]any{{"ID": "2", "Name": "second", "ParentID": "parent", "Category": 6, "Size": "20", "IsInRecycleBin": true}},
		}}})
	}))
	defer server.Close()

	items, err := newTestClient(server, tokens).ListTrash(context.Background(), 2)
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if len(items) != 2 || items[0].ID != "1" || items[1].ID != "2" || requests.Load() != 2 {
		t.Fatalf("unexpected result: items=%v requests=%d", items, requests.Load())
	}
	if !items[0].IsInRecycleBin || !items[1].IsInRecycleBin {
		t.Fatalf("trash items were not marked as in the recycle bin: %#v", items)
	}
}

func TestListTrashListsAllPagesWhenLimitIsZero(t *testing.T) {
	tokens := &testTokens{initial: "token"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		if body == nil {
			return
		}
		variables := body["variables"].(map[string]any)
		if variables["Limit"] != float64(maxLimit) {
			t.Errorf("Limit = %v, want %d", variables["Limit"], maxLimit)
		}
		page := requests.Add(1)
		result := map[string]any{"Items": []map[string]any{{"ID": fmt.Sprint(page), "Name": "item", "Category": 6, "Size": 1, "IsInRecycleBin": true}}}
		if page == 1 {
			result["NextToken"] = "cursor"
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getDeletedFiles": result}})
			return
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getDeletedFiles": result}})
	}))
	defer server.Close()

	items, err := newTestClient(server, tokens).ListTrash(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if len(items) != 2 || items[0].ID != "1" || items[1].ID != "2" || requests.Load() != 2 {
		t.Fatalf("unexpected result: items=%v requests=%d", items, requests.Load())
	}
}

func TestListTrashRejectsNonRecycledItem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getDeletedFiles": map[string]any{
			"Items": []map[string]any{{"ID": "1", "Name": "active", "Category": 6, "Size": 1, "IsInRecycleBin": false}},
		}}})
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: "token"}).ListTrash(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "was not marked as in the recycle bin") {
		t.Fatalf("ListTrash error = %v, want non-recycled item rejection", err)
	}
}

func TestListTrashRejectsDuplicateIDsAcrossPages(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		page := requests.Add(1)
		result := map[string]any{"Items": []map[string]any{{"ID": "duplicate", "Name": "item", "Category": 6, "Size": 1, "IsInRecycleBin": true}}}
		if page == 1 {
			result["NextToken"] = "next"
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getDeletedFiles": result}})
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: "token"}).ListTrash(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), `duplicate item ID "duplicate"`) {
		t.Fatalf("ListTrash error = %v, want duplicate ID error", err)
	}
}

func TestListTrashRejectsRepeatedCursor(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		page := requests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getDeletedFiles": map[string]any{
			"Items":     []map[string]any{{"ID": fmt.Sprint(page), "Name": "item", "Category": 6, "Size": 1, "IsInRecycleBin": true}},
			"NextToken": "same",
		}}})
	}))
	defer server.Close()

	_, err := newTestClient(server, &testTokens{initial: "token"}).ListTrash(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "response repeated a pagination cursor") {
		t.Fatalf("ListTrash error = %v, want repeated cursor error", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("ListTrash requests = %d, want 2", requests.Load())
	}
}

func TestDeleteTrashItemsSendsBulkIDsAndAcknowledgement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		if body == nil {
			return
		}
		variables := body["variables"].(map[string]any)
		if variables["IsInRecycleBin"] != true {
			t.Errorf("IsInRecycleBin = %v, want true", variables["IsInRecycleBin"])
		}
		ids := variables["IDs"].([]any)
		if len(ids) != 2 || ids[0].(map[string]any)["FileID"] != "1" || ids[1].(map[string]any)["FileID"] != "2" {
			t.Errorf("IDs = %#v, want bulk trash IDs 1 and 2", ids)
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"setDeleteFile5": true}})
	}))
	defer server.Close()

	if err := newTestClient(server, &testTokens{initial: "token"}).DeleteTrashItems(context.Background(), []string{"1", "2"}); err != nil {
		t.Fatalf("DeleteTrashItems: %v", err)
	}
}

func TestDeleteTrashItemsNoopForEmptyInput(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"setDeleteFile5": true}})
	}))
	defer server.Close()

	if err := newTestClient(server, &testTokens{initial: "token"}).DeleteTrashItems(context.Background(), nil); err != nil {
		t.Fatalf("DeleteTrashItems: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("DeleteTrashItems sent %d requests, want zero", requests.Load())
	}
}
