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
	"strconv"
	"strings"
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
}

func (tokens *testTokens) Token(context.Context) (string, error) {
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

func TestRegisterRefreshesPureUnauthorizedErrorOnce(t *testing.T) {
	tokens := &testTokens{initial: "expired", refreshed: "fresh"}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeGraphQLRequest(t, request)
		variables := body["variables"].(map[string]any)
		requests.Add(1)
		if variables["Token"] == "expired" {
			writeJSON(t, writer, map[string]any{"errors": []any{map[string]any{
				"message": "Unauthorized", "errorType": "UnauthorizedException",
			}}})
			return
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{"setUploadFile3": "registered"}})
	}))
	defer server.Close()

	got, err := newTestClient(server, tokens).Register(context.Background(), "parent", "file.bin", "checksum", 1, time.Now())
	if err != nil || got != "registered" {
		t.Fatalf("Register = %q, %v; want registered, nil", got, err)
	}
	if requests.Load() != 2 || tokens.refreshes.Load() != 1 {
		t.Fatalf("requests/refreshes = %d/%d, want 2/1", requests.Load(), tokens.refreshes.Load())
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
