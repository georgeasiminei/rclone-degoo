package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginStoresTokensWithoutPassword(t *testing.T) {
	const password = "account-password-do-not-save"
	var loginCalls, refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		var request map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		switch r.URL.Path {
		case loginPath:
			loginCalls.Add(1)
			assert.Equal(t, true, request["GenerateToken"])
			assert.Equal(t, "person@example.test", request["Username"])
			assert.Equal(t, password, request["Password"])
			writeJSON(t, w, http.StatusOK, map[string]string{"RefreshToken": "private-refresh-token"})
		case refreshPath:
			refreshCalls.Add(1)
			assert.Equal(t, "private-refresh-token", request["RefreshToken"])
			writeJSON(t, w, http.StatusOK, map[string]string{"Token": testJWT(time.Now().Add(time.Hour))})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	require.NoError(t, saveState(authFile, tokenState{PasswordLoginAfter: time.Now().Add(time.Hour).Unix()}))
	require.NoError(t, Login(context.Background(), server.Client(), server.URL, authFile, "person@example.test", password))

	data, err := ReadPrivateFile(authFile)
	require.NoError(t, err)
	assert.NotContains(t, string(data), password)
	assert.NotContains(t, string(data), "person@example.test")
	assert.NotContains(t, string(data), "retry_after", "explicit login clears an automatic-login cooldown")
	assert.Contains(t, string(data), "private-refresh-token")
	assert.Contains(t, string(data), "\"token\"")
	info, err := os.Stat(authFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Equal(t, int32(1), loginCalls.Load())
	assert.Equal(t, int32(1), refreshCalls.Load())
}

func TestLoginWrongPasswordFailsWithoutRetryOrDisclosure(t *testing.T) {
	const password = "wrong-account-password"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "rejected credential: %s", password)
	}))
	defer server.Close()

	err := Login(context.Background(), server.Client(), server.URL, filepath.Join(privateTempDir(t), "auth.json"), "person@example.test", password)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rejected the supplied credentials")
	assert.NotContains(t, err.Error(), password)
	assert.Equal(t, int32(1), calls.Load())
}

func TestTokenRefreshesExpiredJWTAndPersistsState(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	require.NoError(t, saveState(authFile, tokenState{
		Token:        testJWT(time.Now().Add(-time.Hour)),
		RefreshToken: "saved-refresh-token",
	}))
	var calls atomic.Int32
	newToken := testJWT(time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, refreshPath, r.URL.Path)
		var request map[string]string
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "saved-refresh-token", request["RefreshToken"])
		writeJSON(t, w, http.StatusOK, map[string]string{"AccessToken": newToken})
	}))
	defer server.Close()

	client, err := New(server.Client(), authFile, server.URL)
	require.NoError(t, err)
	token, err := client.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, newToken, token)
	assert.Equal(t, int32(1), calls.Load())
	data, err := ReadPrivateFile(authFile)
	require.NoError(t, err)
	var saved tokenState
	require.NoError(t, json.Unmarshal(data, &saved))
	assert.Equal(t, newToken, saved.Token)
	assert.Equal(t, "saved-refresh-token", saved.RefreshToken)
}

func TestTokenReloadsLoginStateAfterClientCreation(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	require.NoError(t, saveState(authFile, tokenState{
		Token:        testJWT(time.Now().Add(-time.Hour)),
		RefreshToken: "old-refresh-token",
	}))
	var refreshCalls atomic.Int32
	freshToken := testJWT(time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]string
		if r.URL.Path == loginPath {
			var login map[string]any
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&login))
			writeJSON(t, w, http.StatusOK, map[string]string{"RefreshToken": "new-refresh-token"})
			return
		}
		assert.Equal(t, refreshPath, r.URL.Path)
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "new-refresh-token", request["RefreshToken"])
		refreshCalls.Add(1)
		writeJSON(t, w, http.StatusOK, map[string]string{"Token": freshToken})
	}))
	defer server.Close()

	client, err := New(server.Client(), authFile, server.URL)
	require.NoError(t, err)
	require.NoError(t, Login(context.Background(), server.Client(), server.URL, authFile, "person@example.test", "password"))

	token, err := client.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, freshToken, token)
	assert.Equal(t, int32(1), refreshCalls.Load(), "only the login exchange should refresh")
}

func TestRefreshReloadsLoginStateAfterClientCreation(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	oldToken := testJWT(time.Now().Add(time.Hour))
	newToken := testJWT(time.Now().Add(2 * time.Hour))
	require.NoError(t, saveState(authFile, tokenState{Token: oldToken, RefreshToken: "old-refresh-token"}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == loginPath {
			writeJSON(t, w, http.StatusOK, map[string]string{"RefreshToken": "new-refresh-token"})
			return
		}
		if r.URL.Path != refreshPath {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]string{"Token": newToken})
	}))
	defer server.Close()

	client, err := New(server.Client(), authFile, server.URL)
	require.NoError(t, err)
	require.NoError(t, Login(context.Background(), server.Client(), server.URL, authFile, "person@example.test", "password"))

	token, err := client.Refresh(context.Background(), oldToken)
	require.NoError(t, err)
	assert.Equal(t, newToken, token)
}

func TestRefreshRejectionReloadsConcurrentLoginState(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	require.NoError(t, saveState(authFile, tokenState{
		Token:        testJWT(time.Now().Add(-time.Hour)),
		RefreshToken: "old-refresh-token",
	}))
	freshToken := testJWT(time.Now().Add(time.Hour))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != refreshPath {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := saveState(authFile, tokenState{Token: freshToken, RefreshToken: "new-refresh-token"}); err != nil {
			t.Errorf("save concurrent login state: %v", err)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"response body must remain private"}`))
	}))
	defer server.Close()

	client, err := New(server.Client(), authFile, server.URL)
	require.NoError(t, err)
	token, err := client.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, freshToken, token)
	assert.Equal(t, int32(1), calls.Load())
}

func TestRefreshDoesNotReplaceValidCurrentTokenWithStaleFile(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	current := testJWT(time.Now().Add(time.Hour))
	require.NoError(t, saveState(authFile, tokenState{Token: current, RefreshToken: "current-refresh"}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := New(server.Client(), authFile, server.URL)
	require.NoError(t, err)
	require.NoError(t, saveState(authFile, tokenState{Token: testJWT(time.Now().Add(-time.Hour)), RefreshToken: "stale-refresh"}))

	token, err := client.Refresh(context.Background(), "rejected-older-token")
	require.NoError(t, err)
	assert.Equal(t, current, token)
	token, err = client.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, current, token)
}

func TestConcurrentTokenCallsShareOneRefresh(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	require.NoError(t, saveState(authFile, tokenState{
		Token:        testJWT(time.Now().Add(-time.Hour)),
		RefreshToken: "saved-refresh-token",
	}))
	var calls atomic.Int32
	newToken := testJWT(time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		writeJSON(t, w, http.StatusOK, map[string]string{"Token": newToken})
	}))
	defer server.Close()

	client, err := New(server.Client(), authFile, server.URL)
	require.NoError(t, err)
	const count = 12
	results := make(chan string, count)
	errs := make(chan error, count)
	for range count {
		go func() {
			token, err := client.Token(context.Background())
			results <- token
			errs <- err
		}()
	}
	for range count {
		assert.NoError(t, <-errs)
		assert.Equal(t, newToken, <-results)
	}
	assert.Equal(t, int32(1), calls.Load())
}

func TestRefreshReusesNewerCurrentToken(t *testing.T) {
	current := testJWT(time.Now().Add(30 * time.Second))
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	require.NoError(t, saveState(authFile, tokenState{Token: current, RefreshToken: "saved-refresh-token"}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected refresh request")
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := New(server.Client(), authFile, server.URL)
	require.NoError(t, err)
	token, err := client.Refresh(context.Background(), "rejected-old-token")
	require.NoError(t, err)
	assert.Equal(t, current, token)
}

func TestRejectedRefreshRequiresLogin(t *testing.T) {
	authFile := filepath.Join(privateTempDir(t), "auth.json")
	require.NoError(t, saveState(authFile, tokenState{RefreshToken: "expired-refresh-token"}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"refresh token rejected"}`))
	}))
	defer server.Close()

	client, err := New(server.Client(), authFile, server.URL)
	require.NoError(t, err)
	_, err = client.Token(context.Background())
	assert.ErrorIs(t, err, ErrLoginRequired)
	var loginErr *LoginRequiredError
	assert.ErrorAs(t, err, &loginErr)
	assert.Equal(t, http.StatusUnauthorized, loginErr.StatusCode())
	assert.NotContains(t, err.Error(), "refresh token rejected")
}

func TestSavedCredentialsLoginAfterRefreshRejection(t *testing.T) {
	const email = "person@example.test"
	const accountPassword = "account-password-not-in-vault"
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, saveState(authFile, tokenState{RefreshToken: "rejected-refresh"}))
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "salt", map[string]any{"remote": "degoo:backup", "cache_dir": "/tmp/cache"}))
	require.NoError(t, SaveCredentials(vaultFile, email, accountPassword))

	var refreshCalls, loginCalls atomic.Int32
	newToken := testJWT(time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		switch r.URL.Path {
		case refreshPath:
			refreshCalls.Add(1)
			if request["RefreshToken"] == "rejected-refresh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			assert.Equal(t, "login-refresh", request["RefreshToken"])
			writeJSON(t, w, http.StatusOK, map[string]string{"Token": newToken})
		case loginPath:
			loginCalls.Add(1)
			assert.Equal(t, email, request["Username"])
			assert.Equal(t, accountPassword, request["Password"])
			writeJSON(t, w, http.StatusOK, map[string]string{"RefreshToken": "login-refresh"})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewWithCredentials(server.Client(), authFile, server.URL, vaultFile)
	require.NoError(t, err)
	token, err := client.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, newToken, token)
	assert.Equal(t, int32(2), refreshCalls.Load(), "rejected refresh followed by login token exchange")
	assert.Equal(t, int32(1), loginCalls.Load())
	data, err := ReadPrivateFile(authFile)
	require.NoError(t, err)
	assert.NotContains(t, string(data), accountPassword)
	assert.NotContains(t, string(data), email)
}

func TestSavedCredentialsAreNotUsedForValidTokens(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	token := testJWT(time.Now().Add(time.Hour))
	require.NoError(t, saveState(authFile, tokenState{Token: token, RefreshToken: "refresh"}))
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "", nil))
	require.NoError(t, SaveCredentials(vaultFile, "person@example.test", "account-password"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected authentication request to %s", r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := NewWithCredentials(server.Client(), authFile, server.URL, vaultFile)
	require.NoError(t, err)
	got, err := client.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, token, got)
}

func TestSavedCredentialsDoNotFallbackOnRefreshServerFailure(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, saveState(authFile, tokenState{RefreshToken: "refresh"}))
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "", nil))
	require.NoError(t, SaveCredentials(vaultFile, "person@example.test", "account-password"))
	var refreshCalls, loginCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == refreshPath {
			refreshCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		loginCalls.Add(1)
		http.Error(w, "unexpected login", http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := NewWithCredentials(server.Client(), authFile, server.URL, vaultFile)
	require.NoError(t, err)
	_, err = client.Token(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrLoginRequired)
	assert.Equal(t, int32(maxAttempts), refreshCalls.Load())
	assert.Zero(t, loginCalls.Load())
}

func TestSavedCredentialsDoNotFallbackOnRefreshNetworkFailure(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, saveState(authFile, tokenState{RefreshToken: "refresh"}))
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "", nil))
	require.NoError(t, SaveCredentials(vaultFile, "person@example.test", "account-password"))
	var refreshCalls, loginCalls atomic.Int32
	clientHTTP := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == loginPath {
			loginCalls.Add(1)
		} else {
			refreshCalls.Add(1)
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("offline")}
	})}

	client, err := NewWithCredentials(clientHTTP, authFile, "https://auth.example.test", vaultFile)
	require.NoError(t, err)
	_, err = client.Token(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrLoginRequired)
	assert.Equal(t, int32(maxAttempts), refreshCalls.Load())
	assert.Zero(t, loginCalls.Load())
}

func TestSavedCredentialLoginFailurePersistsCooldown(t *testing.T) {
	const accountPassword = "bad-password-secret"
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, saveState(authFile, tokenState{RefreshToken: "rejected-refresh"}))
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "", nil))
	require.NoError(t, SaveCredentials(vaultFile, "person@example.test", accountPassword))
	var refreshCalls, loginCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == refreshPath {
			refreshCalls.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		loginCalls.Add(1)
		time.Sleep(1100 * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "password was %s", accountPassword)
	}))
	defer server.Close()

	client, err := NewWithCredentials(server.Client(), authFile, server.URL, vaultFile)
	require.NoError(t, err)
	_, err = client.Token(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), accountPassword)
	assert.Contains(t, err.Error(), "degoo login")
	assert.Equal(t, int32(1), loginCalls.Load())
	var state tokenState
	data, readErr := ReadPrivateFile(authFile)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &state))
	assert.GreaterOrEqual(t, state.PasswordLoginAfter, time.Now().Add(59*time.Second).Unix())

	_, err = client.Token(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), accountPassword)
	assert.Contains(t, err.Error(), "cooling down")
	assert.Equal(t, int32(1), loginCalls.Load(), "cooldown suppresses another password attempt")
	require.NoError(t, ClearCredentials(vaultFile))
	_, err = client.Token(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "saved account credentials are unavailable")
	assert.Equal(t, int32(1), loginCalls.Load(), "clearing the vault credentials takes effect immediately")
}

func TestSavedCredentialLoginRateLimitPersistsCooldown(t *testing.T) {
	const accountPassword = "rate-limited-password-secret"
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, saveState(authFile, tokenState{RefreshToken: "rejected-refresh"}))
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "", nil))
	require.NoError(t, SaveCredentials(vaultFile, "person@example.test", accountPassword))
	var refreshCalls, loginCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == refreshPath {
			refreshCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		loginCalls.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprintf(w, "account password was %s", accountPassword)
	}))
	defer server.Close()

	client, err := NewWithCredentials(server.Client(), authFile, server.URL, vaultFile)
	require.NoError(t, err)
	_, err = client.Token(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), accountPassword)
	assert.Equal(t, int32(1), loginCalls.Load(), "the password endpoint is not retried automatically")
	var state tokenState
	data, readErr := ReadPrivateFile(authFile)
	require.NoError(t, readErr)
	require.NoError(t, json.Unmarshal(data, &state))
	assert.GreaterOrEqual(t, state.PasswordLoginAfter, time.Now().Add(119*time.Second).Unix(), "provider Retry-After extends the persistent cooldown")
	_, err = client.Token(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cooling down")
	assert.Equal(t, int32(1), loginCalls.Load())
	assert.Equal(t, int32(2), refreshCalls.Load())
}

func TestSavedCredentialExchangeRateLimitPersistsUncappedCooldown(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, saveState(authFile, tokenState{RefreshToken: "rejected-refresh"}))
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "", nil))
	require.NoError(t, SaveCredentials(vaultFile, "person@example.test", "account-password"))
	var refreshCalls, exchangeCalls, loginCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case refreshPath:
			var request map[string]string
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			if request["RefreshToken"] == "password-refresh" {
				exchangeCalls.Add(1)
				w.Header().Set("Retry-After", "600")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			refreshCalls.Add(1)
			w.WriteHeader(http.StatusForbidden)
		case loginPath:
			loginCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]string{"RefreshToken": "password-refresh"})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewWithCredentials(server.Client(), authFile, server.URL, vaultFile)
	require.NoError(t, err)
	_, err = client.Token(context.Background())
	require.Error(t, err)
	assert.Equal(t, int32(1), loginCalls.Load())
	assert.Equal(t, int32(1), exchangeCalls.Load(), "the password token exchange is one-shot")

	var state tokenState
	data, err := ReadPrivateFile(authFile)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &state))
	assert.GreaterOrEqual(t, state.PasswordLoginAfter, time.Now().Add(599*time.Second).Unix(), "the server Retry-After is persisted without the retry sleep cap")

	_, err = client.Token(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cooling down")
	assert.Equal(t, int32(1), loginCalls.Load(), "the persisted cooldown prevents another password attempt")
	assert.Equal(t, int32(2), refreshCalls.Load())
}

func TestLoginAndLogoutShareAuthFileLock(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "", nil))
	loginEntered := make(chan struct{})
	releaseLogin := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case loginPath:
			close(loginEntered)
			<-releaseLogin
			writeJSON(t, w, http.StatusOK, map[string]string{"RefreshToken": "password-refresh"})
		case refreshPath:
			writeJSON(t, w, http.StatusOK, map[string]string{"Token": testJWT(time.Now().Add(time.Hour))})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	loginDone := make(chan error, 1)
	go func() {
		loginDone <- LoginWithCredentials(context.Background(), server.Client(), server.URL, authFile, vaultFile, "person@example.test", "account-password")
	}()
	<-loginEntered
	logoutDone := make(chan error, 1)
	go func() { logoutDone <- LogoutWithCredentials(authFile, vaultFile) }()
	select {
	case err := <-logoutDone:
		t.Fatalf("logout bypassed the in-flight credential login: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseLogin)
	require.NoError(t, <-loginDone)
	require.NoError(t, <-logoutDone)

	_, err := os.Stat(authFile)
	assert.ErrorIs(t, err, os.ErrNotExist)
	fields := readTestVaultFields(t, vaultFile)
	assert.NotContains(t, fields, "account_credentials", "logout clears credentials saved by the preceding login")
}

func TestAuthFileLockHonorsContextCancellation(t *testing.T) {
	authFile := filepath.Join(privateTempDir(t), "auth.json")
	unlock, err := lockAuthFile(context.Background(), authFile)
	require.NoError(t, err)
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	lockResult := make(chan error, 1)
	go func() {
		close(started)
		waiterUnlock, err := lockAuthFile(ctx, authFile)
		if err == nil {
			waiterUnlock()
		}
		lockResult <- err
	}()
	<-started
	cancel()
	select {
	case err := <-lockResult:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("auth lock waiter ignored context cancellation")
	}
}

func TestCooldownWritePreservesCurrentAuthStatusAndTokens(t *testing.T) {
	authFile := filepath.Join(privateTempDir(t), "auth.json")
	require.NoError(t, WritePrivateFile(authFile, []byte(`{"token":"latest-token","refresh_token":"latest-refresh","login_status":"login-required","diagnostic":"preserve me"}`)))
	require.NoError(t, saveRetryAfter(authFile, tokenState{PasswordLoginAfter: 12345}))
	data, err := ReadPrivateFile(authFile)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	for key, expected := range map[string]string{
		"token":         "latest-token",
		"refresh_token": "latest-refresh",
		"login_status":  "login-required",
		"diagnostic":    "preserve me",
	} {
		var actual string
		require.NoError(t, json.Unmarshal(fields[key], &actual))
		assert.Equal(t, expected, actual)
	}
	var retryAfter int64
	require.NoError(t, json.Unmarshal(fields["retry_after"], &retryAfter))
	assert.Equal(t, int64(12345), retryAfter)
}

func TestConcurrentSavedCredentialFallbackUsesOnePasswordLogin(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, saveState(authFile, tokenState{RefreshToken: "rejected-refresh"}))
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "", nil))
	require.NoError(t, SaveCredentials(vaultFile, "person@example.test", "account-password"))
	var refreshCalls, loginCalls atomic.Int32
	newToken := testJWT(time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case refreshPath:
			if refreshCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]string{"Token": newToken})
		case loginPath:
			loginCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			writeJSON(t, w, http.StatusOK, map[string]string{"RefreshToken": "login-refresh"})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewWithCredentials(server.Client(), authFile, server.URL, vaultFile)
	require.NoError(t, err)
	const count = 12
	results := make(chan string, count)
	errs := make(chan error, count)
	for range count {
		go func() {
			token, err := client.Token(context.Background())
			results <- token
			errs <- err
		}()
	}
	for range count {
		assert.NoError(t, <-errs)
		assert.Equal(t, newToken, <-results)
	}
	assert.Equal(t, int32(1), loginCalls.Load())
}

func TestSeparateClientsShareSavedCredentialFallbackLock(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, saveState(authFile, tokenState{RefreshToken: "rejected-refresh"}))
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "", nil))
	require.NoError(t, SaveCredentials(vaultFile, "person@example.test", "account-password"))
	var refreshCalls, loginCalls atomic.Int32
	bothRefreshes := make(chan struct{})
	newToken := testJWT(time.Now().Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		switch r.URL.Path {
		case refreshPath:
			if request["RefreshToken"] == "rejected-refresh" {
				if refreshCalls.Add(1) == 2 {
					close(bothRefreshes)
				}
				<-bothRefreshes
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]string{"Token": newToken})
		case loginPath:
			loginCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			writeJSON(t, w, http.StatusOK, map[string]string{"RefreshToken": "login-refresh"})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	first, err := NewWithCredentials(server.Client(), authFile, server.URL, vaultFile)
	require.NoError(t, err)
	second, err := NewWithCredentials(server.Client(), authFile, server.URL, vaultFile)
	require.NoError(t, err)
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for _, client := range []*Client{first, second} {
		go func(client *Client) {
			token, err := client.Token(context.Background())
			results <- token
			errs <- err
		}(client)
	}
	for range 2 {
		assert.NoError(t, <-errs)
		assert.Equal(t, newToken, <-results)
	}
	assert.Equal(t, int32(2), refreshCalls.Load())
	assert.Equal(t, int32(1), loginCalls.Load())
}

func TestSavedCredentialsEncryptedRoundTripRejectsWrongKeyAndTampering(t *testing.T) {
	const email = "private@example.test"
	const password = "private-account-password"
	dir := privateTempDir(t)
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "vault-salt", map[string]any{"remote": "degoo:backup"}))
	require.NoError(t, SaveCredentials(vaultFile, email, password))

	data, err := ReadPrivateFile(vaultFile)
	require.NoError(t, err)
	assert.NotContains(t, string(data), email)
	assert.NotContains(t, string(data), password)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	var encrypted string
	require.NoError(t, json.Unmarshal(fields["account_credentials"], &encrypted))
	assert.NotEmpty(t, encrypted)
	assert.NotContains(t, encrypted, email)
	assert.NotContains(t, encrypted, password)

	credentials, err := readCredentials(vaultFile)
	require.NoError(t, err)
	assert.Equal(t, email, credentials.email)
	assert.Equal(t, password, credentials.password)

	require.NoError(t, writeCredentialVault(t, vaultFile, "wrong-vault-password", "vault-salt", map[string]any{"remote": "degoo:backup"}))
	fields = readTestVaultFields(t, vaultFile)
	fields["account_credentials"] = json.RawMessage(strconv.Quote(encrypted))
	require.NoError(t, writeTestVaultFields(vaultFile, fields))
	_, err = readCredentials(vaultFile)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), password)

	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "vault-salt", map[string]any{"remote": "degoo:backup"}))
	require.NoError(t, SaveCredentials(vaultFile, email, password))
	fields = readTestVaultFields(t, vaultFile)
	require.NoError(t, json.Unmarshal(fields["account_credentials"], &encrypted))
	encoded, err := base64.StdEncoding.DecodeString(encrypted)
	require.NoError(t, err)
	encoded[len(encoded)-1] ^= 0xff
	fields["account_credentials"] = json.RawMessage(strconv.Quote(base64.StdEncoding.EncodeToString(encoded)))
	require.NoError(t, writeTestVaultFields(vaultFile, fields))
	_, err = readCredentials(vaultFile)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), password)
}

func TestClearCredentialsPreservesOtherVaultFields(t *testing.T) {
	dir := privateTempDir(t)
	vaultFile := filepath.Join(dir, "vault.json")
	require.NoError(t, writeCredentialVault(t, vaultFile, "vault-password", "salt", map[string]any{"remote": "degoo:backup", "cache_dir": "/private/cache", "future_option": map[string]any{"enabled": true}}))
	require.NoError(t, SaveCredentials(vaultFile, "person@example.test", "account-password"))
	require.NoError(t, ClearCredentials(vaultFile))

	fields := readTestVaultFields(t, vaultFile)
	assert.NotContains(t, fields, "account_credentials")
	var remote, password, salt, cacheDir string
	require.NoError(t, json.Unmarshal(fields["remote"], &remote))
	require.NoError(t, json.Unmarshal(fields["password"], &password))
	require.NoError(t, json.Unmarshal(fields["password_salt"], &salt))
	require.NoError(t, json.Unmarshal(fields["cache_dir"], &cacheDir))
	assert.Equal(t, "degoo:backup", remote)
	assert.Equal(t, "vault-password", password)
	assert.Equal(t, "salt", salt)
	assert.Equal(t, "/private/cache", cacheDir)
	assert.Contains(t, fields, "future_option")
}

func TestLogoutOnlyRemovesAuthFile(t *testing.T) {
	dir := privateTempDir(t)
	authFile := filepath.Join(dir, "auth.json")
	secretFile := filepath.Join(dir, "vault.secret")
	require.NoError(t, WritePrivateFile(authFile, []byte(`{"token":"account-token"}`)))
	require.NoError(t, WritePrivateFile(secretFile, []byte("vault-secret")))

	require.NoError(t, Logout(authFile))
	_, err := os.Stat(authFile)
	assert.ErrorIs(t, err, os.ErrNotExist)
	secret, err := ReadPrivateFile(secretFile)
	require.NoError(t, err)
	assert.Equal(t, "vault-secret", string(secret))
}

func TestPrivateFilePermissionsAndSymlinks(t *testing.T) {
	t.Run("permissive file", func(t *testing.T) {
		dir := privateTempDir(t)
		path := filepath.Join(dir, "public.json")
		require.NoError(t, os.WriteFile(path, []byte("secret"), 0o600))
		require.NoError(t, os.Chmod(path, 0o644))
		_, err := ReadPrivateFile(path)
		assert.ErrorContains(t, err, "owner-only")
	})

	t.Run("permissive parent", func(t *testing.T) {
		parent := filepath.Join(privateTempDir(t), "shared")
		require.NoError(t, os.Mkdir(parent, 0o755))
		path := filepath.Join(parent, "auth.json")
		require.NoError(t, os.WriteFile(path, []byte("secret"), 0o600))
		_, err := ReadPrivateFile(path)
		assert.ErrorContains(t, err, "directory must be owner-only")
	})

	t.Run("file symlink", func(t *testing.T) {
		dir := privateTempDir(t)
		target := filepath.Join(dir, "target")
		require.NoError(t, WritePrivateFile(target, []byte("secret")))
		link := filepath.Join(dir, "auth.json")
		require.NoError(t, os.Symlink(target, link))
		_, err := ReadPrivateFile(link)
		assert.ErrorContains(t, err, "symlink")
	})

	t.Run("parent symlink", func(t *testing.T) {
		dir := privateTempDir(t)
		targetDir := filepath.Join(dir, "private")
		require.NoError(t, os.Mkdir(targetDir, 0o700))
		target := filepath.Join(targetDir, "auth.json")
		require.NoError(t, WritePrivateFile(target, []byte("secret")))
		linkDir := filepath.Join(dir, "alias")
		require.NoError(t, os.Symlink(targetDir, linkDir))
		_, err := ReadPrivateFile(filepath.Join(linkDir, "auth.json"))
		assert.ErrorContains(t, err, "symlink")
	})
}

func TestWritePrivateFileCreatesPrivatePathAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state", "auth.json")
	require.NoError(t, WritePrivateFile(path, []byte("state")))
	data, err := ReadPrivateFile(path)
	require.NoError(t, err)
	assert.Equal(t, "state", string(data))
	fileInfo, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fileInfo.Mode().Perm())
	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestAtomicWriteFailurePreservesPreviousState(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "auth.json")
	require.NoError(t, WritePrivateFile(path, []byte("old-state")))

	err := writePrivateFile(path, []byte("new-state"), func(string, string) error {
		return errors.New("injected rename failure")
	})
	require.Error(t, err)
	data, readErr := ReadPrivateFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "old-state", string(data))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "auth.json", entries[0].Name())
}

func TestAuthenticationRetriesAreBoundedAndHonorRetryAfter(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	api := rest.NewClient(server.Client()).SetRoot(server.URL)
	status, _, err := requestJSON(context.Background(), api, loginPath, map[string]string{"Username": "user"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, int32(maxAttempts), calls.Load())

	resp := &http.Response{Header: http.Header{"Retry-After": []string{"3"}}}
	assert.Equal(t, 3*time.Second, retryDelay(resp, 0))
	resp.Header.Set("Retry-After", "999999")
	assert.Equal(t, maxRetryAfter, retryDelay(resp, 0))
}

func TestAuthenticationResponseBodyIsBounded(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, maxResponseSize+1))
	}))
	defer server.Close()

	api := rest.NewClient(server.Client()).SetRoot(server.URL)
	_, body, err := requestJSON(context.Background(), api, loginPath, map[string]string{"Username": "user"})
	assert.Error(t, err)
	assert.Nil(t, body)
	assert.Equal(t, int32(1), calls.Load())
}

func TestAuthenticationEndpointRequiresTLSExceptLoopback(t *testing.T) {
	_, err := normalizeBaseURL("http://auth.example.test")
	assert.ErrorContains(t, err, "requires HTTPS")
	_, err = normalizeBaseURL("http://127.0.0.1:1234")
	assert.NoError(t, err)
	_, err = normalizeBaseURL("https://auth.example.test")
	assert.NoError(t, err)
}

func testJWT(expiry time.Time) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]int64{"exp": expiry.Unix()})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	assert.NoError(t, json.NewEncoder(w).Encode(data))
}

func TestTokenExpiredUsesMarginAndTreatsMalformedAsExpired(t *testing.T) {
	now := time.Now()
	assert.True(t, tokenExpired(testJWT(now.Add(30*time.Second)), now))
	assert.False(t, tokenExpired(testJWT(now.Add(2*time.Minute)), now))
	assert.True(t, tokenExpired("not-a-jwt", now))
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.Mkdir(dir, 0o700))
	return dir
}

func writeCredentialVault(t *testing.T, path, password, salt string, extra map[string]any) error {
	t.Helper()
	fields := map[string]any{
		"version":   1,
		"remote":    "degoo:backup",
		"password":  password,
		"cache_dir": "/private/cache",
	}
	if salt != "" {
		fields["password_salt"] = salt
	}
	for key, value := range extra {
		fields[key] = value
	}
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivateFile(path, append(data, '\n'))
}

func readTestVaultFields(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := ReadPrivateFile(path)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	return fields
}

func writeTestVaultFields(path string, fields map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivateFile(path, append(data, '\n'))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
