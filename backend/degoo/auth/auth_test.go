package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	require.NoError(t, Login(context.Background(), server.Client(), server.URL, authFile, "person@example.test", password))

	data, err := ReadPrivateFile(authFile)
	require.NoError(t, err)
	assert.NotContains(t, string(data), password)
	assert.NotContains(t, string(data), "person@example.test")
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
