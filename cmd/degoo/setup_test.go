//go:build linux

package degoo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/backend/degoo/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAncestorFolders(t *testing.T) {
	items := map[string]api.Item{
		"vault":  {ID: "vault", Name: "Private vault", ParentID: "device", Category: 2, URL: "secret-signed-url"},
		"device": {ID: "device", Name: "Device", ParentID: "0", Category: 1},
	}
	get := func(_ context.Context, id string) (api.Item, error) { return items[id], nil }
	entries, err := ancestorFolders(context.Background(), get, "vault")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "Device", entries[0].Name)
	assert.Equal(t, "Private vault", entries[1].Name)
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "secret-signed-url")
	items["device"] = api.Item{ID: "device", ParentID: "vault"}
	_, err = ancestorFolders(context.Background(), get, "vault")
	require.ErrorContains(t, err, "cyclic")
	_, err = ancestorFolders(context.Background(), get, "missing")
	require.ErrorContains(t, err, "mismatched")
	_, err = ancestorFolders(context.Background(), func(context.Context, string) (api.Item, error) { return api.Item{}, errors.New("unavailable") }, "vault")
	require.EqualError(t, err, `inspect ancestor folder ID "vault": unavailable`)
}

func TestWriteFolders(t *testing.T) {
	var out bytes.Buffer
	err := writeFolders(&out, []api.Item{
		{ID: "1", Name: "Device", Category: 1, URL: "secret-url"},
		{ID: "2", Name: "Vault", Category: 2},
		{ID: "3", Name: "Native", Category: 6},
		{ID: "4", Name: "Personal file", Category: 0, URL: "secret-url"},
	})
	require.NoError(t, err)
	var entries []folderLocation
	require.NoError(t, json.Unmarshal(out.Bytes(), &entries))
	require.Len(t, entries, 3)
	assert.NotContains(t, out.String(), "secret-url")
	assert.NotContains(t, out.String(), "Personal file")
}

func TestValidSetupName(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "a\\b", "a\x00b", " Vault", ".rclone-degoo-upload-danger"} {
		assert.False(t, validSetupName(name), name)
	}
	assert.True(t, validSetupName("Private vault"))
}

func TestSetupAPIFailsFastDuringPersistedCooldown(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path == "/access-token/v2" {
			_ = json.NewEncoder(writer).Encode(map[string]string{"Token": "refreshed-token", "RefreshToken": "refresh-token"})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"getDeletedFiles": map[string]any{"Items": []any{}}}})
	}))
	defer server.Close()

	authFile := filepath.Join(t.TempDir(), "private", "auth.json")
	require.NoError(t, auth.WritePrivateFile(authFile, []byte(`{"token":"","refresh_token":"refresh-token"}`)))
	until := time.Now().Add(time.Hour).Unix() + 1
	state, err := json.Marshal(struct {
		Version   int   `json:"version"`
		UntilUnix int64 `json:"until_unix"`
	}{Version: 1, UntilUnix: until})
	require.NoError(t, err)
	require.NoError(t, auth.WritePrivateFile(authFile+".rate-limit.json", state))

	m := transportDefaults()
	m["auth_file"] = authFile
	m["api_url"], m["rest_url"] = server.URL, server.URL
	client, err := setupAPI(context.Background(), m)
	require.NoError(t, err)
	_, err = client.ListTrash(context.Background(), 1)
	var rateLimitErr *api.RateLimitError
	require.ErrorAs(t, err, &rateLimitErr)
	assert.Equal(t, until, rateLimitErr.Until.Unix())
	assert.False(t, api.Retryable(err))
	assert.Equal(t, http.StatusTooManyRequests, api.HTTPStatus(err))
	assert.Zero(t, requests.Load(), "cooldown must block both token refresh and GraphQL requests")
}

func TestSetupAPIRejectsInvalidRateLimitSidecar(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	authFile := filepath.Join(t.TempDir(), "private", "auth.json")
	require.NoError(t, auth.WritePrivateFile(authFile, []byte(`{"token":"","refresh_token":"refresh-token"}`)))
	require.NoError(t, auth.WritePrivateFile(authFile+".rate-limit.json", []byte(`{"version":1,"until_unix":"invalid"}`)))

	m := transportDefaults()
	m["auth_file"] = authFile
	m["api_url"], m["rest_url"] = server.URL, server.URL
	client, err := setupAPI(context.Background(), m)
	assert.Nil(t, client)
	require.ErrorContains(t, err, "rate limit sidecar is invalid")
	assert.Zero(t, requests.Load(), "invalid local state must fail before any network request")
}
