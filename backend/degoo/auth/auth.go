// Package auth manages Degoo account tokens and their private local storage.
package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/rest"
)

const (
	defaultBaseURL  = "https://rest-api.degoo.com"
	loginPath       = "/login"
	refreshPath     = "/access-token/v2"
	tokenExpirySkew = 60 * time.Second
	maxResponseSize = 1 << 20
	maxAttempts     = 3
	maxRetryAfter   = 5 * time.Minute
)

// ErrLoginRequired indicates that no usable account token can be obtained
// without an explicit login.
var ErrLoginRequired = errors.New("degoo login required")

type tokenState struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
}

// Client obtains and refreshes Degoo access tokens.
type Client struct {
	mu       sync.Mutex
	api      *rest.Client
	authFile string
	state    tokenState
}

// New creates a token client and loads private token state when it exists.
// A missing auth file is allowed; Token then returns ErrLoginRequired.
func New(client *http.Client, authFile, baseURL string) (*Client, error) {
	baseURL, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if authFile == "" {
		return nil, errors.New("degoo auth file path is required")
	}
	authFile, err = filepath.Abs(authFile)
	if err != nil {
		return nil, fmt.Errorf("resolve Degoo auth file path: %w", err)
	}
	if client == nil {
		client = http.DefaultClient
	}

	c := &Client{
		api:      rest.NewClient(client).SetRoot(baseURL),
		authFile: authFile,
	}
	data, err := ReadPrivateFile(authFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, fmt.Errorf("read Degoo auth state: %w", err)
	}
	if err := json.Unmarshal(data, &c.state); err != nil {
		return nil, errors.New("degoo auth state is invalid")
	}
	return c, nil
}

// Token returns a valid access token, refreshing it when it is expired or
// close to expiry. ErrLoginRequired means the user must run the login flow.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Token != "" && !tokenExpired(c.state.Token, time.Now()) {
		return c.state.Token, nil
	}
	return c.refreshLocked(ctx)
}

// Refresh exchanges the saved refresh token after a server rejects an access
// token. If another caller already replaced rejectedToken, its token is reused.
func (c *Client) Refresh(ctx context.Context, rejectedToken string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Token != "" && c.state.Token != rejectedToken {
		return c.state.Token, nil
	}
	return c.refreshLocked(ctx)
}

func (c *Client) refreshLocked(ctx context.Context) (string, error) {
	if c.state.RefreshToken == "" {
		return "", ErrLoginRequired
	}

	status, body, err := c.requestJSON(ctx, refreshPath, map[string]string{
		"RefreshToken": c.state.RefreshToken,
	})
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusBadRequest {
		return "", fmt.Errorf("%w: saved Degoo authentication was rejected", ErrLoginRequired)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("degoo token refresh failed (HTTP %d)", status)
	}
	var response struct {
		Token        string `json:"Token"`
		AccessToken  string `json:"AccessToken"`
		RefreshToken string `json:"RefreshToken"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", errors.New("degoo token refresh returned an invalid response")
	}
	token := response.Token
	if token == "" {
		token = response.AccessToken
	}
	if token == "" {
		return "", errors.New("degoo token refresh returned no access token")
	}

	state := tokenState{Token: token, RefreshToken: c.state.RefreshToken}
	if response.RefreshToken != "" {
		state.RefreshToken = response.RefreshToken
	}
	if err := saveState(c.authFile, state); err != nil {
		return "", fmt.Errorf("save Degoo auth state: %w", err)
	}
	c.state = state
	return token, nil
}

// Login authenticates with an account password and saves only the returned
// access and refresh tokens. The password is never persisted.
func Login(ctx context.Context, client *http.Client, baseURL, authFile, email, password string) error {
	if email == "" || password == "" {
		return errors.New("degoo email and password are required")
	}
	if authFile == "" {
		return errors.New("degoo auth file path is required")
	}
	authFile, err := filepath.Abs(authFile)
	if err != nil {
		return fmt.Errorf("resolve Degoo auth file path: %w", err)
	}
	api, err := newAPI(client, baseURL)
	if err != nil {
		return err
	}

	status, body, err := requestJSON(ctx, api, loginPath, map[string]any{
		"GenerateToken": true,
		"Username":      email,
		"Password":      password,
	})
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return errors.New("degoo login rejected the supplied credentials")
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("degoo login failed (HTTP %d)", status)
	}
	var response struct {
		Token        string `json:"Token"`
		RefreshToken string `json:"RefreshToken"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return errors.New("degoo login returned an invalid response")
	}
	refreshToken := response.RefreshToken
	if refreshToken == "" {
		refreshToken = response.Token
	}
	if refreshToken == "" {
		return errors.New("degoo login returned no refresh token")
	}

	status, body, err = requestJSON(ctx, api, refreshPath, map[string]string{
		"RefreshToken": refreshToken,
	})
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("degoo token exchange failed (HTTP %d)", status)
	}
	var tokenResponse struct {
		Token        string `json:"Token"`
		AccessToken  string `json:"AccessToken"`
		RefreshToken string `json:"RefreshToken"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return errors.New("degoo token exchange returned an invalid response")
	}
	token := tokenResponse.Token
	if token == "" {
		token = tokenResponse.AccessToken
	}
	if token == "" {
		return errors.New("degoo token exchange returned no access token")
	}
	if tokenResponse.RefreshToken != "" {
		refreshToken = tokenResponse.RefreshToken
	}
	if err := saveState(authFile, tokenState{Token: token, RefreshToken: refreshToken}); err != nil {
		return fmt.Errorf("save Degoo auth state: %w", err)
	}
	return nil
}

// Logout removes the Degoo auth file and leaves other local secrets untouched.
func Logout(authFile string) error {
	if authFile == "" {
		return errors.New("degoo auth file path is required")
	}
	absolute, err := filepath.Abs(authFile)
	if err != nil {
		return fmt.Errorf("resolve Degoo auth file path: %w", err)
	}
	dir, err := checkPrivateParent(absolute, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if _, err := privateFileInfo(absolute); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(absolute); err != nil {
		return fmt.Errorf("remove Degoo auth state: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync Degoo auth directory: %w", err)
	}
	return nil
}

// ReadPrivateFile reads a regular owner-private file without following symlinks.
// Parent directories must be real directories and the containing directory
// must have no group or other permissions.
func ReadPrivateFile(path string) ([]byte, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve private file path: %w", err)
	}
	if _, err := checkPrivateParent(absolute, false); err != nil {
		return nil, err
	}
	before, err := privateFileInfo(absolute)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(absolute)
	if err != nil {
		return nil, fmt.Errorf("open private file: %w", err)
	}
	defer func() { _ = f.Close() }()
	after, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat private file: %w", err)
	}
	if !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm()&0o077 != 0 || after.Mode().Perm()&0o400 == 0 {
		return nil, errors.New("private file changed or has unsafe permissions")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read private file: %w", err)
	}
	if len(data) > maxResponseSize {
		return nil, errors.New("private file is too large")
	}
	return data, nil
}

// WritePrivateFile atomically writes owner-private data and syncs the file and
// containing directory before returning.
func WritePrivateFile(path string, data []byte) error {
	return writePrivateFile(path, data, os.Rename)
}

func writePrivateFile(path string, data []byte, rename func(string, string) error) error {
	if path == "" {
		return errors.New("private file path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve private file path: %w", err)
	}
	dir, err := checkPrivateParent(absolute, true)
	if err != nil {
		return err
	}
	if _, err := privateFileInfo(absolute); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".degoo-auth-*")
	if err != nil {
		return fmt.Errorf("create private file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set private file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write private file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync private file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close private file: %w", err)
	}
	if err := rename(tmpName, absolute); err != nil {
		return fmt.Errorf("replace private file: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync private file directory: %w", err)
	}
	return nil
}

func saveState(path string, state tokenState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return WritePrivateFile(path, data)
}

func newAPI(client *http.Client, baseURL string) (*rest.Client, error) {
	baseURL, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return rest.NewClient(client).SetRoot(baseURL), nil
}

func normalizeBaseURL(baseURL string) (string, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid Degoo authentication endpoint")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return "", errors.New("degoo authentication requires HTTPS outside localhost tests")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return strings.TrimSuffix(u.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) requestJSON(ctx context.Context, path string, request any) (int, []byte, error) {
	return requestJSON(ctx, c.api, path, request)
}

func requestJSON(ctx context.Context, api *rest.Client, path string, request any) (int, []byte, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return 0, nil, errors.New("could not encode Degoo authentication request")
	}
	defer clear(payload)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		resp, callErr := api.Call(ctx, &rest.Opts{
			Method:       http.MethodPost,
			Path:         path,
			Body:         bytes.NewReader(payload),
			ContentType:  "application/json",
			IgnoreStatus: true,
			NoRedirect:   true,
		})
		if callErr != nil {
			if attempt+1 < maxAttempts && isTransientNetworkError(callErr) {
				if err := waitContext(ctx, backoff(attempt)); err != nil {
					return 0, nil, err
				}
				continue
			}
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 0, nil, fmt.Errorf("degoo authentication request failed: %w", callErr)
		}
		if resp == nil {
			return 0, nil, errors.New("degoo authentication request returned no response")
		}
		status := resp.StatusCode
		body, readErr := readResponseBody(resp)
		if readErr != nil {
			return status, nil, errors.New("could not read Degoo authentication response")
		}
		if isRetryStatus(status) && attempt+1 < maxAttempts {
			if err := waitContext(ctx, retryDelay(resp, attempt)); err != nil {
				return 0, nil, err
			}
			continue
		}
		return status, body, nil
	}
	return 0, nil, errors.New("degoo authentication request exhausted retries")
}

func readResponseBody(resp *http.Response) (body []byte, err error) {
	defer fs.CheckClose(resp.Body, &err)
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err == nil && len(body) > maxResponseSize {
		return nil, rest.ErrBodyTooLarge
	}
	return body, err
}

func isRetryStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isTransientNetworkError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func retryDelay(resp *http.Response, attempt int) time.Duration {
	if resp != nil {
		header := strings.TrimSpace(resp.Header.Get("Retry-After"))
		if seconds, err := strconv.ParseInt(header, 10, 64); err == nil && seconds >= 0 {
			if seconds >= int64(maxRetryAfter/time.Second) {
				return maxRetryAfter
			}
			return time.Duration(seconds) * time.Second
		}
		if retryAt, err := http.ParseTime(header); err == nil {
			return min(max(time.Until(retryAt), 0), maxRetryAfter)
		}
	}
	return backoff(attempt)
}

func backoff(attempt int) time.Duration {
	return min(100*time.Millisecond*(1<<attempt), 2*time.Second)
}

func waitContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func tokenExpired(token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return true
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return true
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return true
	}
	var expiry json.Number
	if err := json.Unmarshal(claims["exp"], &expiry); err != nil {
		return true
	}
	seconds, err := expiry.Float64()
	if err != nil {
		return true
	}
	return time.UnixMilli(int64(seconds * 1000)).Before(now.Add(tokenExpirySkew))
}

func checkPrivateParent(path string, create bool) (string, error) {
	dir := filepath.Dir(path)
	if create {
		for current := dir; ; current = filepath.Dir(current) {
			info, err := os.Lstat(current)
			if err == nil {
				if info.Mode()&os.ModeSymlink != 0 {
					return "", errors.New("private file path contains a symlink")
				}
				if !info.IsDir() {
					return "", errors.New("private file parent is not a directory")
				}
				break
			}
			if !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("inspect private file directory: %w", err)
			}
			if filepath.Dir(current) == current {
				return "", fmt.Errorf("inspect private file directory: %w", err)
			}
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create private file directory: %w", err)
		}
	}
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("inspect private file directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("private file path contains a symlink")
		}
		if !info.IsDir() {
			return "", errors.New("private file parent is not a directory")
		}
		if current == dir && (info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o700 != 0o700) {
			return "", errors.New("private file parent directory must be owner-only and writable")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return dir, nil
}

func privateFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("private file must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("private file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return nil, errors.New("private file must be owner-readable and owner-only")
	}
	return info, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
