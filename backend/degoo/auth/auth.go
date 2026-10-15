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
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
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
	loginCooldown   = time.Minute
)

// ErrLoginRequired indicates that no usable account token can be obtained
// without an explicit login.
var ErrLoginRequired = errors.New("degoo login required")

// LoginRequiredError indicates that saved account authentication cannot be
// renewed and the user must log in again.
type LoginRequiredError struct {
	statusCode int
}

// Error returns a sanitized login-required diagnostic.
func (err *LoginRequiredError) Error() string {
	if err == nil || err.statusCode == 0 {
		return ErrLoginRequired.Error()
	}
	return fmt.Sprintf("%s (auth HTTP %d)", ErrLoginRequired, err.statusCode)
}

// Unwrap identifies this error as ErrLoginRequired.
func (err *LoginRequiredError) Unwrap() error { return ErrLoginRequired }

// StatusCode returns the authentication endpoint status, or zero if none was
// received.
func (err *LoginRequiredError) StatusCode() int {
	if err == nil {
		return 0
	}
	return err.statusCode
}

func loginRequired(statusCode int) error {
	return &LoginRequiredError{statusCode: statusCode}
}

type tokenState struct {
	Token              string `json:"token"`
	RefreshToken       string `json:"refresh_token"`
	PasswordLoginAfter int64  `json:"retry_after,omitempty"`
}

// Client obtains and refreshes Degoo access tokens.
type Client struct {
	mu        sync.Mutex
	api       *rest.Client
	authFile  string
	vaultFile string
	state     tokenState
}

type authFileLock struct {
	semaphore  chan struct{}
	references int
}

var authFileLockRegistry = struct {
	sync.Mutex
	locks map[string]*authFileLock
}{locks: make(map[string]*authFileLock)}

// lockAuthFile coordinates clients that share an auth file.
func lockAuthFile(ctx context.Context, path string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve Degoo auth file path: %w", err)
	}
	authFileLockRegistry.Lock()
	lock := authFileLockRegistry.locks[absolute]
	if lock == nil {
		lock = &authFileLock{semaphore: make(chan struct{}, 1)}
		authFileLockRegistry.locks[absolute] = lock
	}
	lock.references++
	authFileLockRegistry.Unlock()

	select {
	case lock.semaphore <- struct{}{}:
	case <-ctx.Done():
		authFileLockRegistry.Lock()
		lock.references--
		if lock.references == 0 {
			delete(authFileLockRegistry.locks, absolute)
		}
		authFileLockRegistry.Unlock()
		return nil, ctx.Err()
	}
	releaseProcessLock := func() {
		<-lock.semaphore
		authFileLockRegistry.Lock()
		lock.references--
		if lock.references == 0 {
			delete(authFileLockRegistry.locks, absolute)
		}
		authFileLockRegistry.Unlock()
	}
	releaseOSLock, err := lockAuthFileOS(ctx, absolute)
	if err != nil {
		releaseProcessLock()
		return nil, err
	}
	return func() {
		releaseOSLock()
		releaseProcessLock()
	}, nil
}

// WithPrivateFileLock runs fn while holding the cross-process lock associated
// with path. The lock file is a private sibling of path.
func WithPrivateFileLock(ctx context.Context, path string, fn func() error) error {
	if fn == nil {
		return errors.New("private file lock callback is nil")
	}
	unlock, err := lockAuthFile(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

type loginAttemptError struct {
	statusCode int
	retryAfter time.Time
}

func (err *loginAttemptError) Error() string {
	if err.statusCode == http.StatusUnauthorized || err.statusCode == http.StatusForbidden {
		return "degoo login rejected the supplied credentials"
	}
	if err.statusCode != 0 {
		return fmt.Sprintf("degoo login failed (HTTP %d)", err.statusCode)
	}
	return "degoo login failed"
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
	state, exists, err := readState(authFile)
	if err != nil {
		return nil, fmt.Errorf("read Degoo auth state: %w", err)
	}
	if exists {
		c.state = state
	}
	return c, nil
}

// NewWithCredentials creates a token client that can renew rejected account
// authentication using credentials encrypted in a private vault file. An
// empty vault file keeps the behavior of New.
func NewWithCredentials(client *http.Client, authFile, baseURL, vaultFile string) (*Client, error) {
	c, err := New(client, authFile, baseURL)
	if err != nil {
		return nil, err
	}
	if vaultFile != "" {
		vaultFile, err = filepath.Abs(vaultFile)
		if err != nil {
			return nil, errors.New("resolve Degoo vault file path")
		}
		c.vaultFile = vaultFile
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
	if _, err := c.reloadStateLocked(""); err != nil {
		return "", fmt.Errorf("reload Degoo auth state: %w", err)
	}
	if c.state.Token != "" && !tokenExpired(c.state.Token, time.Now()) {
		return c.state.Token, nil
	}
	token, err := c.refreshLocked(ctx)
	if errors.Is(err, ErrLoginRequired) {
		return c.loginWithSavedCredentialsLocked(ctx, err)
	}
	return token, err
}

// Refresh exchanges the saved refresh token after a server rejects an access
// token. If another caller already replaced rejectedToken, its token is reused.
func (c *Client) Refresh(ctx context.Context, rejectedToken string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Token != "" && c.state.Token != rejectedToken {
		return c.state.Token, nil
	}
	if _, err := c.reloadStateLocked(rejectedToken); err != nil {
		return "", fmt.Errorf("reload Degoo auth state: %w", err)
	}
	if c.state.Token != "" && c.state.Token != rejectedToken {
		return c.state.Token, nil
	}
	token, err := c.refreshLocked(ctx)
	if errors.Is(err, ErrLoginRequired) {
		return c.loginWithSavedCredentialsLocked(ctx, err)
	}
	return token, err
}

func (c *Client) loginWithSavedCredentialsLocked(ctx context.Context, loginRequiredErr error) (string, error) {
	if c.vaultFile == "" {
		return "", loginRequiredErr
	}
	unlock, err := lockAuthFile(ctx, c.authFile)
	if err != nil {
		return "", fmt.Errorf("lock Degoo auth state: %w", err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	// A login completed by another process may have replaced the rejected
	// state while this client was waiting for the password endpoint.
	latest, exists, err := readState(c.authFile)
	if err != nil {
		return "", fmt.Errorf("%w: reload Degoo auth state: %v", loginRequiredErr, err)
	}
	if exists {
		if latest.Token != "" && latest.Token != c.state.Token && !tokenExpired(latest.Token, time.Now()) {
			c.state = latest
			return c.state.Token, nil
		}
		if latest.PasswordLoginAfter > c.state.PasswordLoginAfter {
			c.state.PasswordLoginAfter = latest.PasswordLoginAfter
		}
	}
	credentials, err := readCredentials(c.vaultFile)
	if err != nil {
		return "", fmt.Errorf("%w: saved account credentials are unavailable or cannot be decrypted; run degoo login", loginRequiredErr)
	}
	if retryAfter := time.Unix(c.state.PasswordLoginAfter, 0); c.state.PasswordLoginAfter != 0 && retryAfter.After(time.Now()) {
		return "", fmt.Errorf("%w: automatic password login is cooling down until %s; run degoo login", loginRequiredErr, retryAfter.Local().Format(time.RFC3339))
	}

	c.state.PasswordLoginAfter = time.Now().Add(loginCooldown).Unix() + 1
	if err := saveRetryAfter(c.authFile, c.state); err != nil {
		return "", fmt.Errorf("%w: could not save the account login cooldown; run degoo login", loginRequiredErr)
	}
	if err := login(ctx, c.api, c.authFile, credentials.email, credentials.password); err != nil {
		cooldownUntil := time.Now().Add(loginCooldown)
		var attemptErr *loginAttemptError
		if errors.As(err, &attemptErr) && attemptErr.retryAfter.After(cooldownUntil) {
			cooldownUntil = attemptErr.retryAfter
		}
		c.state.PasswordLoginAfter = max(c.state.PasswordLoginAfter, unixSecondCeil(cooldownUntil))
		if cooldownErr := saveRetryAfter(c.authFile, c.state); cooldownErr != nil {
			return "", fmt.Errorf("%w: automatic account login failed and its cooldown could not be saved; run degoo login", loginRequiredErr)
		}
		return "", fmt.Errorf("%w: automatic account login failed; run degoo login: %v", loginRequiredErr, err)
	}
	fs.Logf(nil, "degoo saved-account login succeeded")

	state, exists, err := readState(c.authFile)
	if err != nil {
		return "", fmt.Errorf("reload Degoo auth state after login: %w", err)
	}
	if !exists || state.Token == "" {
		return "", errors.New("degoo login completed without saving an access token")
	}
	c.state = state
	if !tokenExpired(c.state.Token, time.Now()) {
		return c.state.Token, nil
	}
	unlock()
	locked = false
	return c.refreshLocked(ctx)
}

// reloadStateLocked reads a different, unexpired private auth state when the
// in-memory access token has expired or the server has rejected it. It never
// replaces a usable in-memory token unless that token was rejected.
func (c *Client) reloadStateLocked(rejectedToken string) (bool, error) {
	currentExpired := c.state.Token == "" || tokenExpired(c.state.Token, time.Now())
	currentRejected := rejectedToken != "" && c.state.Token == rejectedToken
	if !currentExpired && !currentRejected {
		return false, nil
	}
	state, exists, err := readState(c.authFile)
	if err != nil || !exists || state == c.state {
		return false, err
	}
	if state.Token == "" || tokenExpired(state.Token, time.Now()) {
		return false, nil
	}
	c.state = state
	return true, nil
}

func (c *Client) refreshLocked(ctx context.Context) (string, error) {
	if c.state.RefreshToken == "" {
		return "", ErrLoginRequired
	}
	previousState := c.state

	status, body, err := c.requestJSON(ctx, refreshPath, map[string]string{
		"RefreshToken": c.state.RefreshToken,
	})
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusBadRequest {
		if _, err := c.reloadStateLocked(previousState.Token); err != nil {
			return "", fmt.Errorf("reload Degoo auth state: %w", err)
		}
		if c.state != previousState && c.state.Token != "" && !tokenExpired(c.state.Token, time.Now()) {
			return c.state.Token, nil
		}
		return "", loginRequired(status)
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
	unlock, err := lockAuthFile(ctx, c.authFile)
	if err != nil {
		return "", fmt.Errorf("lock Degoo auth state: %w", err)
	}
	defer unlock()
	if savedState, exists, err := readState(c.authFile); err != nil {
		return "", fmt.Errorf("reload Degoo auth state: %w", err)
	} else if !exists {
		return "", ErrLoginRequired
	} else if savedState != previousState && savedState.Token != "" && !tokenExpired(savedState.Token, time.Now()) {
		c.state = savedState
		return savedState.Token, nil
	}
	if err := saveState(c.authFile, state); err != nil {
		return "", fmt.Errorf("save Degoo auth state: %w", err)
	}
	c.state = state
	return token, nil
}

func readState(path string) (tokenState, bool, error) {
	data, err := ReadPrivateFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return tokenState{}, false, nil
		}
		return tokenState{}, false, err
	}
	var state tokenState
	if err := json.Unmarshal(data, &state); err != nil {
		return tokenState{}, false, errors.New("degoo auth state is invalid")
	}
	return state, true, nil
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
	unlock, err := lockAuthFile(ctx, authFile)
	if err != nil {
		return fmt.Errorf("lock Degoo auth state: %w", err)
	}
	defer unlock()
	return login(ctx, api, authFile, email, password)
}

// LoginWithCredentials authenticates and saves encrypted account credentials
// under the same auth lock as the resulting token state. If saving credentials
// fails, the successful login may already have updated the token state.
func LoginWithCredentials(ctx context.Context, client *http.Client, baseURL, authFile, vaultFile, email, password string) error {
	if email == "" || password == "" {
		return errors.New("degoo email and password are required")
	}
	if authFile == "" {
		return errors.New("degoo auth file path is required")
	}
	if vaultFile == "" {
		return errors.New("degoo vault file path is required")
	}
	authFile, err := filepath.Abs(authFile)
	if err != nil {
		return fmt.Errorf("resolve Degoo auth file path: %w", err)
	}
	api, err := newAPI(client, baseURL)
	if err != nil {
		return err
	}
	unlock, err := lockAuthFile(ctx, authFile)
	if err != nil {
		return fmt.Errorf("lock Degoo auth state: %w", err)
	}
	defer unlock()
	if err := login(ctx, api, authFile, email, password); err != nil {
		return err
	}
	if err := saveCredentials(vaultFile, email, password); err != nil {
		return fmt.Errorf("save encrypted Degoo account credentials: %w", err)
	}
	return nil
}

func login(ctx context.Context, api *rest.Client, authFile, email, password string) error {
	status, body, retryAfter, err := requestJSONOnce(ctx, api, loginPath, map[string]any{
		"GenerateToken": true,
		"Username":      email,
		"Password":      password,
	})
	if err != nil {
		if retryAfter.IsZero() {
			return err
		}
		return &loginAttemptError{statusCode: status, retryAfter: retryAfter}
	}
	if status < 200 || status >= 300 {
		return &loginAttemptError{statusCode: status, retryAfter: retryAfter}
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

	status, body, retryAfter, err = requestJSONOnce(ctx, api, refreshPath, map[string]string{
		"RefreshToken": refreshToken,
	})
	if err != nil {
		if retryAfter.IsZero() {
			return err
		}
		return &loginAttemptError{statusCode: status, retryAfter: retryAfter}
	}
	if status < 200 || status >= 300 {
		return &loginAttemptError{statusCode: status, retryAfter: retryAfter}
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
	unlock, err := lockAuthFile(context.Background(), absolute)
	if err != nil {
		return fmt.Errorf("lock Degoo auth state: %w", err)
	}
	defer unlock()
	return logoutLocked(absolute)
}

// LogoutWithCredentials removes saved account credentials and account tokens
// while holding the auth lock so automatic login cannot restore either file.
func LogoutWithCredentials(authFile, vaultFile string) error {
	if authFile == "" {
		return errors.New("degoo auth file path is required")
	}
	if vaultFile == "" {
		return errors.New("degoo vault file path is required")
	}
	absolute, err := filepath.Abs(authFile)
	if err != nil {
		return fmt.Errorf("resolve Degoo auth file path: %w", err)
	}
	unlock, err := lockAuthFile(context.Background(), absolute)
	if err != nil {
		return fmt.Errorf("lock Degoo auth state: %w", err)
	}
	defer unlock()
	if err := clearCredentials(vaultFile); err != nil {
		return err
	}
	return logoutLocked(absolute)
}

func logoutLocked(absolute string) error {
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

type accountCredentials struct {
	email    string
	password string
}

type encryptedAccountCredentials struct {
	Version  int    `json:"version"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// SaveCredentials encrypts Degoo account credentials with the vault cipher
// and stores them in the private vault configuration.
func SaveCredentials(vaultFile, email, password string) error {
	return saveCredentials(vaultFile, email, password)
}

func saveCredentials(vaultFile, email, password string) error {
	if strings.TrimSpace(email) == "" || password == "" {
		return errors.New("degoo email and account password are required")
	}
	fields, err := readVaultFields(vaultFile)
	if err != nil {
		return err
	}
	vaultPassword, vaultSalt, err := vaultSecrets(fields)
	if err != nil {
		return err
	}
	secret := encryptedAccountCredentials{Version: 1, Email: email, Password: password}
	plaintext, err := json.Marshal(secret)
	if err != nil {
		return errors.New("could not encode Degoo account credentials")
	}
	defer clear(plaintext)
	cipher, err := vaultCipher(vaultPassword, vaultSalt)
	if err != nil {
		return errors.New("could not prepare the vault cipher for account credentials")
	}
	stream, err := cipher.EncryptData(bytes.NewReader(plaintext))
	if err != nil {
		return errors.New("could not encrypt Degoo account credentials")
	}
	encrypted, err := io.ReadAll(io.LimitReader(stream, maxResponseSize+1))
	if err != nil || len(encrypted) > maxResponseSize {
		clear(encrypted)
		return errors.New("could not encrypt Degoo account credentials")
	}
	fields["account_credentials"], err = json.Marshal(base64.StdEncoding.EncodeToString(encrypted))
	clear(encrypted)
	if err != nil {
		return errors.New("could not encode encrypted Degoo account credentials")
	}
	return writeVaultFields(vaultFile, fields)
}

// ClearCredentials removes saved Degoo account credentials from a private
// vault configuration while preserving its other fields.
func ClearCredentials(vaultFile string) error {
	return clearCredentials(vaultFile)
}

func clearCredentials(vaultFile string) error {
	fields, err := readVaultFields(vaultFile)
	if err != nil {
		return err
	}
	if _, exists := fields["account_credentials"]; !exists {
		return nil
	}
	delete(fields, "account_credentials")
	return writeVaultFields(vaultFile, fields)
}

func readCredentials(vaultFile string) (accountCredentials, error) {
	fields, err := readVaultFields(vaultFile)
	if err != nil {
		return accountCredentials{}, errors.New("could not read private vault credentials")
	}
	vaultPassword, vaultSalt, err := vaultSecrets(fields)
	if err != nil {
		return accountCredentials{}, errors.New("vault password is unavailable")
	}
	var encoded string
	if err := json.Unmarshal(fields["account_credentials"], &encoded); err != nil || encoded == "" {
		return accountCredentials{}, errors.New("saved account credentials are unavailable")
	}
	encrypted, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(encrypted) > maxResponseSize {
		return accountCredentials{}, errors.New("saved account credentials are invalid")
	}
	cipher, err := vaultCipher(vaultPassword, vaultSalt)
	if err != nil {
		return accountCredentials{}, errors.New("vault cipher is unavailable")
	}
	decrypted, err := cipher.DecryptData(io.NopCloser(bytes.NewReader(encrypted)))
	if err != nil {
		clear(encrypted)
		return accountCredentials{}, errors.New("saved account credentials cannot be decrypted")
	}
	defer clear(encrypted)
	plaintext, readErr := io.ReadAll(io.LimitReader(decrypted, maxResponseSize+1))
	closeErr := decrypted.Close()
	if readErr != nil || closeErr != nil || len(plaintext) > maxResponseSize {
		clear(plaintext)
		return accountCredentials{}, errors.New("saved account credentials cannot be decrypted")
	}
	defer clear(plaintext)
	var secret encryptedAccountCredentials
	if err := json.Unmarshal(plaintext, &secret); err != nil || secret.Version != 1 || secret.Email == "" || secret.Password == "" {
		return accountCredentials{}, errors.New("saved account credentials are invalid")
	}
	return accountCredentials{email: secret.Email, password: secret.Password}, nil
}

func vaultCipher(password, salt string) (*crypt.Cipher, error) {
	obscuredPassword, err := obscure.Obscure(password)
	if err != nil {
		return nil, err
	}
	obscuredSalt, err := obscure.Obscure(salt)
	if err != nil {
		return nil, err
	}
	return crypt.NewCipher(configmap.Simple{
		"password":                  obscuredPassword,
		"password2":                 obscuredSalt,
		"filename_encryption":       "standard",
		"filename_encoding":         "base32",
		"directory_name_encryption": "true",
		"suffix":                    ".bin",
	})
}

func readVaultFields(vaultFile string) (map[string]json.RawMessage, error) {
	if vaultFile == "" {
		return nil, errors.New("degoo vault file path is required")
	}
	data, err := ReadPrivateFile(vaultFile)
	if err != nil {
		return nil, fmt.Errorf("read private Degoo vault configuration: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errors.New("degoo vault configuration is invalid")
	}
	return fields, nil
}

func vaultSecrets(fields map[string]json.RawMessage) (password, salt string, err error) {
	if err := json.Unmarshal(fields["password"], &password); err != nil || password == "" {
		return "", "", errors.New("vault password is unavailable")
	}
	if raw, exists := fields["password_salt"]; exists {
		if err := json.Unmarshal(raw, &salt); err != nil {
			return "", "", errors.New("vault password salt is invalid")
		}
	}
	return password, salt, nil
}

func writeVaultFields(vaultFile string, fields map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return errors.New("could not encode Degoo vault configuration")
	}
	return WritePrivateFile(vaultFile, append(data, '\n'))
}

func saveRetryAfter(path string, state tokenState) error {
	data, err := ReadPrivateFile(path)
	var fields map[string]json.RawMessage
	if errors.Is(err, os.ErrNotExist) {
		data, err = json.Marshal(state)
	} else if err == nil {
		err = json.Unmarshal(data, &fields)
		if err == nil && fields == nil {
			err = errors.New("invalid auth state")
		}
	}
	if err != nil {
		return errors.New("could not read account auth state for cooldown")
	}
	if fields == nil {
		if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
			return errors.New("could not encode account auth cooldown")
		}
	}
	encoded, err := json.Marshal(state.PasswordLoginAfter)
	if err != nil {
		return errors.New("could not encode account auth cooldown")
	}
	fields["retry_after"] = encoded
	data, err = json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return errors.New("could not encode account auth cooldown")
	}
	return WritePrivateFile(path, append(data, '\n'))
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

func requestJSONOnce(ctx context.Context, api *rest.Client, path string, request any) (int, []byte, time.Time, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return 0, nil, time.Time{}, errors.New("could not encode Degoo authentication request")
	}
	defer clear(payload)

	resp, err := api.Call(ctx, &rest.Opts{
		Method:       http.MethodPost,
		Path:         path,
		Body:         bytes.NewReader(payload),
		ContentType:  "application/json",
		IgnoreStatus: true,
		NoRedirect:   true,
	})
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, time.Time{}, ctx.Err()
		}
		return 0, nil, time.Time{}, fmt.Errorf("degoo authentication request failed: %w", err)
	}
	if resp == nil {
		return 0, nil, time.Time{}, errors.New("degoo authentication request returned no response")
	}
	status := resp.StatusCode
	retryAfter, _ := responseRetryAfterAt(resp, time.Now())
	body, err := readResponseBody(resp)
	if err != nil {
		return status, nil, retryAfter, errors.New("could not read Degoo authentication response")
	}
	return status, body, retryAfter, nil
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
	if delay, found := responseRetryAfter(resp); found {
		return delay
	}
	return backoff(attempt)
}

func responseRetryAfter(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	header := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if seconds, err := strconv.ParseUint(header, 10, 64); err == nil {
		if seconds >= uint64(maxRetryAfter/time.Second) {
			return maxRetryAfter, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if retryAt, err := http.ParseTime(header); err == nil {
		return min(max(time.Until(retryAt), 0), maxRetryAfter), true
	}
	return 0, false
}

func responseRetryAfterAt(resp *http.Response, now time.Time) (time.Time, bool) {
	if resp == nil {
		return time.Time{}, false
	}
	header := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if seconds, err := strconv.ParseUint(header, 10, 64); err == nil {
		base := now.Unix()
		if seconds > uint64(math.MaxInt64) || base > math.MaxInt64-int64(seconds) {
			return time.Unix(math.MaxInt64, 0), true
		}
		return time.Unix(base+int64(seconds), 0), true
	}
	if retryAt, err := http.ParseTime(header); err == nil {
		return retryAt, true
	}
	return time.Time{}, false
}

func unixSecondCeil(value time.Time) int64 {
	seconds := value.Unix()
	if value.Nanosecond() != 0 && seconds < math.MaxInt64 {
		seconds++
	}
	return seconds
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
