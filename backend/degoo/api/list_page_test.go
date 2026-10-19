package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListDirPageReturnsOneBoundedPageAndCursor(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		var body struct {
			OperationName string `json:"operationName"`
			Variables     struct {
				ParentID  string `json:"ParentID"`
				Limit     int    `json:"Limit"`
				Order     int    `json:"Order"`
				NextToken string `json:"NextToken"`
			} `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		require.Equal(t, "GetFileChildren5", body.OperationName)
		require.Equal(t, "parent", body.Variables.ParentID)
		require.Equal(t, 2, body.Variables.Limit)
		require.Equal(t, 3, body.Variables.Order)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
			"Items": []map[string]any{
				{"ID": "one", "ParentID": "parent", "Name": "encrypted-one", "Category": 6, "Size": 100},
				{"ID": "two", "ParentID": "parent", "Name": "encrypted-two", "Category": 6, "Size": 100},
			},
			"NextToken": "next-page",
		}}})
	}))
	defer server.Close()

	client := newTestClient(server, &testTokens{initial: "token"})
	items, next, err := client.ListDirPage(context.Background(), "parent", 2, "")
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "next-page", next)
	require.EqualValues(t, 1, calls.Load(), "page helper must not follow the cursor")
}

func TestListDirPageRejectsInvalidLimitBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})

	for _, limit := range []int{-1, 0, maxLimit + 1} {
		_, _, err := client.ListDirPage(context.Background(), "parent", limit, "")
		require.Error(t, err)
	}
	require.Zero(t, calls.Load())
}

func TestListDirPageRejectsDuplicateOrEmptyItemIDs(t *testing.T) {
	for _, items := range [][]map[string]any{
		{{"ID": "duplicate", "ParentID": "parent", "Category": 6, "Size": 1}, {"ID": "duplicate", "ParentID": "parent", "Category": 6, "Size": 1}},
		{{"ID": "", "ParentID": "parent", "Category": 6, "Size": 1}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": items}}})
		}))
		client := newTestClient(server, &testTokens{initial: "token"})
		_, _, err := client.ListDirPage(context.Background(), "parent", 8, "")
		server.Close()
		require.Error(t, err)
	}
}

func TestListDirPageRejectsMissingOrNullListing(t *testing.T) {
	for _, data := range []map[string]any{
		{},
		{"getFileChildren5": nil},
		{"getFileChildren5": map[string]any{"NextToken": "next"}},
		{"getFileChildren5": map[string]any{"Items": nil}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(t, writer, map[string]any{"data": data})
		}))
		client := newTestClient(server, &testTokens{initial: "token"})
		_, _, err := client.ListDirPage(context.Background(), "parent", 8, "")
		server.Close()
		require.Error(t, err)
	}
}

func TestListDirPagePassesCursorAndRejectsOversizedCursor(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		var body struct {
			Variables struct {
				NextToken string `json:"NextToken"`
			} `json:"variables"`
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		require.Equal(t, "opaque-cursor", body.Variables.NextToken)
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": []any{}}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})

	items, next, err := client.ListDirPage(context.Background(), "parent", 4, "opaque-cursor")
	require.NoError(t, err)
	require.Empty(t, items)
	require.Empty(t, next)
	require.EqualValues(t, 1, calls.Load())

	_, _, err = client.ListDirPage(context.Background(), "parent", 4, string(make([]byte, 4097)))
	require.Error(t, err)
	require.EqualValues(t, 1, calls.Load(), "an oversized cursor must be rejected before making a request")
}

func TestListDirPageRejectsMoreItemsThanRequested(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{
			"Items": []map[string]any{
				{"ID": "one", "ParentID": "parent", "Name": "one", "Category": 6, "Size": 1},
				{"ID": "two", "ParentID": "parent", "Name": "two", "Category": 6, "Size": 1},
			},
		}}})
	}))
	defer server.Close()
	client := newTestClient(server, &testTokens{initial: "token"})
	_, _, err := client.ListDirPage(context.Background(), "parent", 1, "")
	require.Error(t, err)
}
