package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetItemRejectsWrongIdentity(t *testing.T) {
	for _, returnedID := range []string{"", "other-generated-id"} {
		t.Run("returned_"+returnedID, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					Operation string                               `json:"operationName"`
					Variables struct{ ID struct{ FileID string } } `json:"variables"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					http.Error(w, "invalid fixture", 400)
					return
				}
				if input.Operation != "GetOverlay4" || input.Variables.ID.FileID != "generated-target" {
					t.Errorf("unexpected request: %+v", input)
					http.Error(w, "unexpected fixture request", 400)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"getOverlay4": map[string]any{"ID": returnedID, "ParentID": "generated-parent", "Name": "generated-name", "Category": 6}}})
			}))
			defer server.Close()
			client := newTestClient(server, &testTokens{initial: "generated-token"})
			item, err := client.GetItem(context.Background(), "generated-target")
			var apiErr *Error
			if !errors.As(err, &apiErr) || item.ID != "" {
				t.Fatalf("wrong identity returned item=%+v err=%v", item, err)
			}
			if len(client.itemParents) != 0 {
				t.Fatal("untrusted identity entered parent hints")
			}
			stats := client.RequestStats()
			if stats.HTTPRequests != 1 || stats.MetadataPages != 0 || stats.RequestsByOperation["GetOverlay4"] != 1 {
				t.Fatalf("unexpected identity failure calls: %+v", stats)
			}
		})
	}
}

func TestGetItemMissingAndForbiddenDoNotList(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"data":{"getOverlay4":null}}`))
			}))
			defer server.Close()
			client := newTestClient(server, &testTokens{initial: "generated-token"})
			_, err := client.GetItem(context.Background(), "generated-target")
			var apiErr *Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("missing/forbidden error = %v", err)
			}
			stats := client.RequestStats()
			if stats.HTTPRequests != 1 || stats.MetadataPages != 0 {
				t.Fatalf("failure fell back to listing or retried: %+v", stats)
			}
			if len(client.itemParents) != 0 {
				t.Fatal("failed overlay entered parent hints")
			}
		})
	}
}
