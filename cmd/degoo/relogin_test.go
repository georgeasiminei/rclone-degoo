//go:build linux

package degoo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/rclone/rclone/backend/degoo/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginAndLogoutWithVaultConfig(t *testing.T) {
	const (
		email    = "person@example.test"
		password = "private-account-password"
	)
	var loginCalls, refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode authentication request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/login":
			loginCalls.Add(1)
			assert.Equal(t, email, request["Username"])
			assert.Equal(t, password, request["Password"])
			_ = json.NewEncoder(w).Encode(map[string]string{"RefreshToken": "refresh-token"})
		case "/access-token/v2":
			refreshCalls.Add(1)
			if request["RefreshToken"] == "rejected-refresh-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"Token": "access-token", "RefreshToken": "refresh-token"})
		default:
			t.Errorf("unexpected authentication path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := filepath.Join(t.TempDir(), "private")
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	cfg := &VaultConfig{
		Version:  1,
		Remote:   "TestDegoo:",
		Password: "separate-vault-password",
		CacheDir: filepath.Join(dir, "cache"),
	}
	require.NoError(t, cfg.validate())
	vaultData, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, auth.WritePrivateFile(vaultFile, vaultData))

	require.NoError(t, loginAccount(context.Background(), server.Client(), server.URL, authFile, vaultFile, email, password))
	require.EqualValues(t, 1, loginCalls.Load())
	require.EqualValues(t, 1, refreshCalls.Load())

	savedData, err := auth.ReadPrivateFile(vaultFile)
	require.NoError(t, err)
	assert.NotContains(t, string(savedData), email)
	assert.NotContains(t, string(savedData), password)
	saved, err := readConfig(vaultFile)
	require.NoError(t, err)
	require.NotEmpty(t, saved.AccountCredentials)
	require.Equal(t, cfg.Password, saved.Password)

	// A rejected saved refresh token exercises the credentials encrypted in the vault file.
	require.NoError(t, auth.WritePrivateFile(authFile, []byte(`{"token":"","refresh_token":"rejected-refresh-token"}`)))
	client, err := auth.NewWithCredentials(server.Client(), authFile, server.URL, vaultFile)
	require.NoError(t, err)
	token, err := client.Token(context.Background())
	require.NoError(t, err)
	require.Equal(t, "access-token", token)
	require.EqualValues(t, 2, loginCalls.Load(), "saved account credentials should renew a rejected refresh token")
	require.GreaterOrEqual(t, refreshCalls.Load(), int32(3), "refresh rejection and credential login exchange should both occur")

	require.NoError(t, logoutAccount(authFile, ""), "logout discovers the sibling vault configuration by default")
	_, err = auth.ReadPrivateFile(authFile)
	require.Error(t, err)
	cleared, err := readConfig(vaultFile)
	require.NoError(t, err)
	require.Empty(t, cleared.AccountCredentials)
	require.Equal(t, cfg.Password, cleared.Password, "logout keeps the vault secret")
}

func TestLogoutWithInvalidVaultConfigKeepsAuthState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, auth.WritePrivateFile(authFile, []byte(`{"token":"access-token","refresh_token":"refresh-token"}`)))
	require.NoError(t, auth.WritePrivateFile(vaultFile, []byte(`{"version":1}`)))

	require.Error(t, logoutAccount(authFile, vaultFile))
	_, err := auth.ReadPrivateFile(authFile)
	require.NoError(t, err, "invalid vault configuration must stop before account tokens are removed")
}

func TestVaultCredentialFileOptionOverridesRemoteValue(t *testing.T) {
	remote := ":degoo,auth_file=/tmp/degoo-auth.json,root_folder_id=vault,credentials_file=/tmp/old-vault.json:dir"
	fsInfo, configName, fsPath, config, err := mountRawConfig(remote, true, "/tmp/new-vault.json")
	require.NoError(t, err)
	require.Equal(t, "degoo", fsInfo.Name)
	require.Equal(t, ":degoo", configName)
	require.Equal(t, "dir", fsPath)
	got, ok := config.Get("credentials_file")
	require.True(t, ok)
	require.Equal(t, "/tmp/new-vault.json", got)
}

func TestLegacyMountRawConfigNeedsNoCredentialOption(t *testing.T) {
	remote := ":degoo,auth_file=/tmp/degoo-auth.json,root_folder_id=vault:"
	_, _, _, config, err := mountRawConfig(remote, true)
	require.NoError(t, err)
	got, ok := config.Get("credentials_file")
	require.True(t, ok)
	require.Empty(t, got, "an older remote without the optional credential path remains valid")
}
