//go:build linux

package degoo

import (
	"bytes"
	"context"
	"encoding/base64"
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
	rfs "github.com/rclone/rclone/fs"
	"github.com/spf13/cobra"
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

func TestLocationRootOnlyBypassesAncestorFailure(t *testing.T) {
	var calls atomic.Int32
	var failAncestor atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Variables struct {
				ID struct {
					FileID string `json:"FileID"`
				} `json:"ID"`
			} `json:"variables"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Errorf("decode location request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		if input.Variables.ID.FileID == "vault" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"getOverlay4": map[string]any{
				"ID": "vault", "Name": "Private vault", "ParentID": "device", "Category": 2,
			}}})
			return
		}
		if !failAncestor.Load() && input.Variables.ID.FileID == "device" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"getOverlay4": map[string]any{
				"ID": "device", "Name": "Device", "ParentID": "0", "Category": 1,
			}}})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"errors": []any{map[string]any{
			"message": "ancestor is unavailable", "errorType": "GraphQLException",
		}}})
	}))
	defer server.Close()
	vaultFile := writeTestLocationVault(t, server.URL)

	output, err := runLocation(context.Background(), t, vaultFile)
	require.NoError(t, err)
	var got []folderLocation
	require.NoError(t, json.Unmarshal(output, &got))
	require.Equal(t, []folderLocation{
		{ID: "device", Name: "Device", Category: 1},
		{ID: "vault", Name: "Private vault", Category: 2},
	}, got, "default location must keep returning the full root-to-leaf ancestry")
	assert.EqualValues(t, 2, calls.Load())

	calls.Store(0)
	failAncestor.Store(true)
	output, err = runLocation(context.Background(), t, vaultFile, "--root-only")
	require.NoError(t, err)
	got = nil
	require.NoError(t, json.Unmarshal(output, &got))
	require.Equal(t, []folderLocation{{ID: "vault", Name: "Private vault", Category: 2}}, got)
	assert.EqualValues(t, 1, calls.Load(), "root-only must issue one GetItem request and skip the parent")

	calls.Store(0)
	output, err = runLocation(context.Background(), t, vaultFile)
	require.ErrorContains(t, err, "inspect ancestor folder ID \"device\"")
	assert.EqualValues(t, 2, calls.Load(), "default location must continue resolving the configured root and its parent")
	assert.Empty(t, output, "a partial ancestor chain must not be reported as complete")
}

func TestLocationRootOnlyPropagatesBadRoot(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"getOverlay4": nil}})
	}))
	defer server.Close()
	vaultFile := writeTestLocationVault(t, server.URL)

	output, err := runLocation(context.Background(), t, vaultFile, "--root-only")
	require.ErrorContains(t, err, "inspect root folder ID \"vault\"")
	assert.ErrorContains(t, err, "not found")
	assert.EqualValues(t, 1, calls.Load(), "an invalid root must fail after its single direct lookup")
	assert.Empty(t, output)
}

func TestLocationRootOnlyHonorsCancellationBeforeLookup(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	vaultFile := writeTestLocationVault(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runLocation(ctx, t, vaultFile, "--root-only")
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, calls.Load(), "a canceled preflight must not start a provider request")
}

func writeTestLocationVault(t *testing.T, apiURL string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	authFile := filepath.Join(dir, "auth.json")
	claims, err := json.Marshal(map[string]int64{"exp": time.Now().Add(time.Hour).Unix()})
	require.NoError(t, err)
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	state, err := json.Marshal(map[string]string{"token": token, "refresh_token": "unused"})
	require.NoError(t, err)
	require.NoError(t, auth.WritePrivateFile(authFile, state))

	const remote = "degoo-location-test"
	for key, value := range map[string]string{
		"type": "degoo", "auth_file": authFile, "root_folder_id": "vault",
		"api_key": "test-key", "api_url": apiURL, "rest_url": apiURL,
	} {
		t.Setenv(rfs.ConfigToEnv(remote, key), value)
	}
	configData, err := json.Marshal(VaultConfig{
		Version: 1, Remote: remote + ":", Password: "generated test vault password",
		CacheDir: filepath.Join(dir, "cache"),
	})
	require.NoError(t, err)
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, auth.WritePrivateFile(vaultFile, configData))
	return vaultFile
}

func runLocation(ctx context.Context, t *testing.T, vaultFile string, flags ...string) ([]byte, error) {
	t.Helper()
	root := &cobra.Command{Use: "degoo", SilenceErrors: true, SilenceUsage: true}
	addSetupCommands(root)
	args := append([]string{"location", vaultFile}, flags...)
	root.SetArgs(args)
	root.SetContext(ctx)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	return output.Bytes(), err
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
