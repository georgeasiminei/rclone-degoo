// Package api implements the Degoo GraphQL and object-storage API used by the backend.
package api

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/degoo/auth"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
)

const maxLimit = 1000
const maxRetryAfter = 5 * time.Minute

// ErrAlreadyExists indicates that Degoo already has the requested content.
var ErrAlreadyExists = errors.New("degoo content already exists")

const itemFields = `
		ID
		Name
		ParentID
		Category
		Size
		URL
		LastModificationTime
		CreationTime
`

const getFileChildrenQuery = `query GetFileChildren5(
	$Token: String!
	$ParentID: String
	$Limit: Int!
	$Order: Int!
	$NextToken: String
) {
	getFileChildren5(
		Token: $Token
		ParentID: $ParentID
		Limit: $Limit
		Order: $Order
		NextToken: $NextToken
	) {
		Items {
` + itemFields + `		}
		NextToken
	}
}`

const getOverlayQuery = `query GetOverlay4($Token: String!, $ID: IDType!) {
	getOverlay4(Token: $Token, ID: $ID) {
` + itemFields + `	}
}`

const getWriteAuthQuery = `query GetBucketWriteAuth4(
	$Token: String!
	$ParentID: String!
	$StorageUploadInfos: [StorageUploadInfo2]
) {
	getBucketWriteAuth4(
		Token: $Token
		ParentID: $ParentID
		StorageUploadInfos: $StorageUploadInfos
	) {
		AuthData {
			PolicyBase64
			Signature
			BaseURL
			KeyPrefix
			AccessKey { Key Value }
			ACL
			AdditionalBody { Key Value }
		}
		Error
	}
}`

const registerMutation = `mutation SetUploadFile3($Token: String!, $FileInfos: [FileInfoUpload3]!) {
	setUploadFile3(Token: $Token, FileInfos: $FileInfos)
}`

const deleteMutation = `mutation SetDeleteFile5($Token: String!, $IsInRecycleBin: Boolean!, $IDs: [IDType]!) {
	setDeleteFile5(Token: $Token, IsInRecycleBin: $IsInRecycleBin, IDs: $IDs)
}`

const moveMutation = `mutation SetMoveFile($Token: String!, $Copy: Boolean, $NewParentID: String!, $FileIDs: [String]!) {
	setMoveFile(Token: $Token, Copy: $Copy, NewParentID: $NewParentID, FileIDs: $FileIDs)
}`

const renameMutation = `mutation SetRenameFile($Token: String!, $FileRenames: [FileRenameInfo]!) {
	setRenameFile(Token: $Token, FileRenames: $FileRenames)
}`

// TokenSource provides and refreshes Degoo account access tokens.
type TokenSource interface {
	// Token returns a current access token.
	Token(context.Context) (string, error)
	// Refresh replaces a rejected token and returns the new access token.
	Refresh(context.Context, string) (string, error)
}

// Client calls Degoo's GraphQL API and uploads or downloads object data.
type Client struct {
	http   *http.Client
	gql    *rest.Client
	tokens TokenSource

	refreshMu sync.Mutex
}

// New creates a Degoo API client. A nil HTTP client uses http.DefaultClient.
func New(httpClient *http.Client, tokens TokenSource, graphqlURL, apiKey string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		http:   httpClient,
		gql:    rest.NewClient(httpClient).SetRoot(graphqlURL).SetHeader("Content-Type", "application/json").SetHeader("x-api-key", apiKey).SetErrorHandler(safeHTTPError),
		tokens: tokens,
	}
}

func safeHTTPError(resp *http.Response) error {
	status := resp.StatusCode
	if _, err := rest.ReadBody(resp); err != nil {
		return fmt.Errorf("http status %d (error response body discarded)", status)
	}
	return fmt.Errorf("http status %d", status)
}

// Item contains the file metadata returned by Degoo.
type Item struct {
	// ID is the provider's identifier for the item.
	ID string
	// Name is the item name.
	Name string
	// ParentID is the provider identifier of the parent.
	ParentID string
	// Category is Degoo's item category code.
	Category int
	// Size is the content length in bytes.
	Size int64
	// URL is a short-lived content download URL.
	URL string
	// LastModificationTime is the provider modification time.
	LastModificationTime time.Time
	// CreationTime is the provider creation time.
	CreationTime time.Time
}

// UnmarshalJSON accepts numeric fields as JSON numbers or decimal strings and
// timestamps as Unix milliseconds or RFC3339 strings.
func (item *Item) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID                   string          `json:"ID"`
		Name                 string          `json:"Name"`
		ParentID             string          `json:"ParentID"`
		Category             json.RawMessage `json:"Category"`
		Size                 json.RawMessage `json:"Size"`
		URL                  string          `json:"URL"`
		LastModificationTime json.RawMessage `json:"LastModificationTime"`
		CreationTime         json.RawMessage `json:"CreationTime"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	category, err := parseInt(wire.Category)
	if err != nil {
		return fmt.Errorf("decode item category: %w", err)
	}
	size, err := parseInt(wire.Size)
	if err != nil {
		return fmt.Errorf("decode item size: %w", err)
	}
	modified, err := parseMillis(wire.LastModificationTime)
	if err != nil {
		return fmt.Errorf("decode item modification time: %w", err)
	}
	created, err := parseMillis(wire.CreationTime)
	if err != nil {
		return fmt.Errorf("decode item creation time: %w", err)
	}
	*item = Item{
		ID:                   wire.ID,
		Name:                 wire.Name,
		ParentID:             wire.ParentID,
		Category:             int(category),
		Size:                 size,
		URL:                  wire.URL,
		LastModificationTime: modified,
		CreationTime:         created,
	}
	return nil
}

func parseInt(value json.RawMessage) (int64, error) {
	if len(value) == 0 || string(value) == "null" {
		return 0, nil
	}
	var number json.Number
	if err := json.Unmarshal(value, &number); err == nil {
		return number.Int64()
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return 0, err
	}
	if text == "" {
		return 0, nil
	}
	return strconv.ParseInt(text, 10, 64)
}

func parseMillis(value json.RawMessage) (time.Time, error) {
	if raw := strings.TrimSpace(string(value)); raw == "" || raw == "null" {
		return time.Time{}, nil
	}
	ms, err := parseInt(value)
	if err == nil {
		if ms == 0 {
			return time.Time{}, nil
		}
		return time.UnixMilli(ms), nil
	}
	var text string
	if stringErr := json.Unmarshal(value, &text); stringErr != nil {
		return time.Time{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}, nil
	}
	if ms, parseErr := strconv.ParseInt(text, 10, 64); parseErr == nil {
		if ms == 0 {
			return time.Time{}, nil
		}
		return time.UnixMilli(ms), nil
	}
	parsed, parseErr := time.Parse(time.RFC3339Nano, text)
	if parseErr != nil {
		return time.Time{}, parseErr
	}
	return parsed, nil
}

// KeyValue represents a named field returned in an upload authorization.
type KeyValue struct {
	// Key is the form field name.
	Key string `json:"Key"`
	// Value is the form field value.
	Value string `json:"Value"`
}

// AuthData contains the fields required for Degoo's GCS form upload.
type AuthData struct {
	// PolicyBase64 is the encoded GCS upload policy.
	PolicyBase64 string `json:"PolicyBase64"`
	// Signature authenticates the policy.
	Signature string `json:"Signature"`
	// BaseURL is the GCS upload endpoint.
	BaseURL string `json:"BaseURL"`
	// KeyPrefix begins the checksum-based object key.
	KeyPrefix string `json:"KeyPrefix"`
	// AccessKey is an optional form credential.
	AccessKey *KeyValue `json:"AccessKey"`
	// ACL is an optional object access control value.
	ACL string `json:"ACL"`
	// AdditionalBody contains optional form fields.
	AdditionalBody []KeyValue `json:"AdditionalBody"`
}

// Error describes an HTTP or GraphQL error returned by Degoo.
type Error struct {
	operation     string
	statusCode    int
	message       string
	graphQLErrors []graphQLError
	err           error
}

// Error returns the API error's message.
func (err *Error) Error() string {
	var message string
	if err.message != "" {
		if err.statusCode != 0 {
			message = fmt.Sprintf("%s: %s (HTTP %d)", err.operation, err.message, err.statusCode)
		} else {
			message = err.operation + ": " + err.message
		}
	} else if err.statusCode != 0 {
		message = fmt.Sprintf("%s: HTTP request failed (status %d)", err.operation, err.statusCode)
	} else {
		message = err.operation + ": Degoo API request failed"
	}
	if diagnostic := authDiagnostic(err.err); diagnostic != "" {
		message += ": " + diagnostic
	}
	return message
}

func authDiagnostic(err error) string {
	if !errors.Is(err, auth.ErrLoginRequired) {
		return ""
	}
	var loginErr *auth.LoginRequiredError
	if errors.As(err, &loginErr) && loginErr.StatusCode() != 0 {
		return fmt.Sprintf("%s (auth HTTP %d)", auth.ErrLoginRequired, loginErr.StatusCode())
	}
	return auth.ErrLoginRequired.Error()
}

// Unwrap returns the underlying HTTP or transport error.
func (err *Error) Unwrap() error { return err.err }

type graphQLError struct {
	message string
	typ     string
}

type graphQLErrorWire struct {
	Message    string         `json:"message"`
	ErrorType  string         `json:"errorType"`
	Type       string         `json:"type"`
	Extensions map[string]any `json:"extensions"`
}

func (wire graphQLErrorWire) normalized() graphQLError {
	rawType := wire.ErrorType
	if rawType == "" {
		rawType = wire.Type
	}
	if rawType == "" && wire.Extensions != nil {
		for _, key := range []string{"errorType", "code", "Code"} {
			if value, ok := wire.Extensions[key].(string); ok {
				rawType = value
				break
			}
		}
	}
	classification := strings.ToLower(rawType + " " + wire.Message)
	typ := "GraphQLException"
	switch {
	case strings.Contains(classification, "unauthor"):
		typ = "UnauthorizedException"
	case strings.Contains(classification, "throttl"), strings.Contains(classification, "too many requests"), strings.Contains(classification, "rate limit"):
		typ = "ThrottlingException"
	case strings.Contains(classification, "serviceunavailable"), strings.Contains(classification, "service unavailable"):
		typ = "ServiceUnavailableException"
	case strings.Contains(classification, "internal"):
		typ = "InternalFailure"
	}
	return graphQLError{message: "GraphQL request failed", typ: typ}
}

type graphQLEnvelope struct {
	Data   json.RawMessage    `json:"data"`
	Errors []graphQLErrorWire `json:"errors"`
}

// HTTPStatus returns the HTTP status attached to an API error, or zero when
// the error did not come from an HTTP response.
func HTTPStatus(err error) int {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.statusCode
	}
	return 0
}

// Retryable reports whether an API or transport error may succeed if retried.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		if apiErr.statusCode == http.StatusTooManyRequests || apiErr.statusCode >= 500 {
			return true
		}
		for _, gqlErr := range apiErr.graphQLErrors {
			typ := strings.ToLower(gqlErr.typ)
			if strings.Contains(typ, "throttl") || strings.Contains(typ, "serviceunavailable") || strings.Contains(typ, "internal") {
				return true
			}
		}
		err = apiErr.err
	}
	return fserrors.ShouldRetry(err)
}

// Checksum returns the Degoo checksum for the seed and first MiB of reader.
// It consumes no more than 1 MiB from reader.
func Checksum(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("degoo checksum: nil content reader")
	}
	hash := sha1.New()
	if _, err := hash.Write([]byte{13, 7, 2, 2, 15, 40, 75, 117, 13, 10, 19, 16, 29, 23, 3, 36}); err != nil {
		return "", fmt.Errorf("degoo checksum: %w", err)
	}
	if _, err := io.Copy(hash, io.LimitReader(reader, 1024*1024)); err != nil {
		return "", fmt.Errorf("degoo checksum: %w", err)
	}
	protobuf := make([]byte, 0, 24)
	protobuf = append(protobuf, 0x0a, 0x14)
	protobuf = append(protobuf, hash.Sum(nil)...)
	protobuf = append(protobuf, 0x10, 0x00)
	return base64.RawURLEncoding.EncodeToString(protobuf), nil
}

func (c *Client) tokenForRequest(ctx context.Context) (string, error) {
	if c.tokens == nil {
		return "", errors.New("degoo token source is nil")
	}
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", errors.New("degoo token source returned an empty token")
	}
	return token, nil
}

func (c *Client) refreshToken(ctx context.Context, used string) (string, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	if c.tokens == nil {
		return "", errors.New("degoo token source is nil")
	}
	current, currentErr := c.tokens.Token(ctx)
	if currentErr == nil && current != "" && current != used {
		return current, nil
	}
	if currentErr != nil {
		return "", currentErr
	}
	refreshed, err := c.tokens.Refresh(ctx, used)
	if err != nil {
		return "", err
	}
	if refreshed == "" {
		return "", errors.New("degoo token refresh returned an empty token")
	}
	return refreshed, nil
}

func retryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(header)
	if err != nil {
		return 0
	}
	d := time.Until(when)
	if d <= 0 {
		return 0
	}
	return min(d, maxRetryAfter)
}

func withRetryAfter(err error, resp *http.Response, mutation bool) error {
	if mutation || resp == nil || !Retryable(err) {
		return err
	}
	if delay := retryAfter(resp.Header.Get("Retry-After")); delay > 0 {
		return pacer.RetryAfterError(err, delay)
	}
	return err
}

func validateUploadBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("degoo upload: invalid storage endpoint")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		host := strings.ToLower(u.Hostname())
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New("degoo upload: storage endpoint must use HTTPS")
}

func isUnauthorized(status int, errs []graphQLError) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	for _, gqlErr := range errs {
		if strings.Contains(strings.ToLower(gqlErr.typ), "unauthor") || strings.Contains(strings.ToLower(gqlErr.message), "unauthorized") {
			return true
		}
	}
	return false
}

func canRefreshGraphQL(mutation bool, data json.RawMessage, errs []graphQLError) bool {
	if !mutation {
		return isUnauthorized(0, errs)
	}
	if data := strings.TrimSpace(string(data)); data != "" && data != "null" {
		return false
	}
	if len(errs) == 0 {
		return false
	}
	for _, gqlErr := range errs {
		if !isUnauthorized(0, []graphQLError{gqlErr}) {
			return false
		}
	}
	return true
}

func (c *Client) graphql(ctx context.Context, operation, query string, variables map[string]any, result any, mutation bool) error {
	token, err := c.tokenForRequest(ctx)
	if err != nil {
		return &Error{operation: operation, message: "cannot obtain access token", err: err}
	}

	for attempt := 0; attempt < 2; attempt++ {
		vars := make(map[string]any, len(variables)+1)
		for key, value := range variables {
			vars[key] = value
		}
		vars["Token"] = token
		request := map[string]any{"query": query, "operationName": operation, "variables": vars}
		var envelope graphQLEnvelope
		resp, callErr := c.gql.CallJSON(ctx, &rest.Opts{
			Method:     http.MethodPost,
			NoRedirect: true, // The access token is in the JSON body and must not be redirected.
		}, request, &envelope)
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		if callErr != nil {
			apiErr := &Error{operation: operation, statusCode: status, message: "HTTP request failed", err: callErr}
			if attempt == 0 && isUnauthorized(status, nil) {
				token, err = c.refreshToken(ctx, token)
				if err != nil {
					return &Error{operation: operation, message: "cannot refresh access token", err: err}
				}
				continue
			}
			return withRetryAfter(apiErr, resp, mutation)
		}
		if len(envelope.Errors) != 0 {
			gqlErrors := make([]graphQLError, 0, len(envelope.Errors))
			for _, wireErr := range envelope.Errors {
				gqlErr := wireErr.normalized()
				gqlErrors = append(gqlErrors, gqlErr)
			}
			apiErr := &Error{operation: operation, message: "GraphQL request failed", graphQLErrors: gqlErrors}
			if attempt == 0 && canRefreshGraphQL(mutation, envelope.Data, gqlErrors) {
				token, err = c.refreshToken(ctx, token)
				if err != nil {
					return &Error{operation: operation, message: "cannot refresh access token", err: err}
				}
				continue
			}
			return withRetryAfter(apiErr, resp, mutation)
		}
		if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return &Error{operation: operation, message: "response did not contain data"}
		}
		if err := json.Unmarshal(envelope.Data, result); err != nil {
			return &Error{operation: operation, message: "decode GraphQL data", err: err}
		}
		return nil
	}
	return &Error{operation: operation, message: "access token remained unauthorized after refresh"}
}

// ListDir returns every item under parentID, requesting at most 1,000 items
// per page. It rejects repeated item IDs and repeated pagination cursors.
func (c *Client) ListDir(ctx context.Context, parentID string) ([]Item, error) {
	items := make([]Item, 0)
	seenIDs := make(map[string]struct{})
	seenTokens := make(map[string]struct{})
	var nextToken string
	for {
		variables := map[string]any{"ParentID": parentID, "Limit": maxLimit, "Order": 3}
		if nextToken != "" {
			variables["NextToken"] = nextToken
		}
		var response struct {
			GetFileChildren5 struct {
				Items     []Item `json:"Items"`
				NextToken string `json:"NextToken"`
			} `json:"getFileChildren5"`
		}
		if err := c.graphql(ctx, "GetFileChildren5", getFileChildrenQuery, variables, &response, false); err != nil {
			return nil, err
		}
		for _, item := range response.GetFileChildren5.Items {
			if item.ID == "" {
				return nil, &Error{operation: "GetFileChildren5", message: "response contained an item with an empty ID"}
			}
			if _, exists := seenIDs[item.ID]; exists {
				return nil, &Error{operation: "GetFileChildren5", message: fmt.Sprintf("duplicate item ID %q across pages", item.ID)}
			}
			seenIDs[item.ID] = struct{}{}
			items = append(items, item)
		}
		nextToken = response.GetFileChildren5.NextToken
		if nextToken == "" {
			return items, nil
		}
		if _, exists := seenTokens[nextToken]; exists {
			return nil, &Error{operation: "GetFileChildren5", message: "response repeated a pagination cursor"}
		}
		seenTokens[nextToken] = struct{}{}
	}
}

// GetItem returns fresh metadata for an item, including its short-lived URL.
func (c *Client) GetItem(ctx context.Context, id string) (Item, error) {
	var response struct {
		GetOverlay4 *Item `json:"getOverlay4"`
	}
	err := c.graphql(ctx, "GetOverlay4", getOverlayQuery, map[string]any{"ID": map[string]string{"FileID": id}}, &response, false)
	if err != nil {
		return Item{}, err
	}
	if response.GetOverlay4 == nil {
		return Item{}, &Error{operation: "GetOverlay4", message: fmt.Sprintf("item %q was not found", id)}
	}
	return *response.GetOverlay4, nil
}

// GetWriteAuth returns the first GCS authorization for an upload.
func (c *Client) GetWriteAuth(ctx context.Context, parentID, name string, size int64, checksum string) (AuthData, error) {
	variables := map[string]any{
		"ParentID": parentID,
		"StorageUploadInfos": []map[string]string{{
			"FileName": name,
			"Checksum": checksum,
			"Size":     strconv.FormatInt(size, 10),
		}},
	}
	var response struct {
		GetBucketWriteAuth4 []struct {
			AuthData *AuthData `json:"AuthData"`
			Error    string    `json:"Error"`
		} `json:"getBucketWriteAuth4"`
	}
	if err := c.graphql(ctx, "GetBucketWriteAuth4", getWriteAuthQuery, variables, &response, false); err != nil {
		return AuthData{}, err
	}
	if len(response.GetBucketWriteAuth4) == 0 {
		return AuthData{}, &Error{operation: "GetBucketWriteAuth4", message: "response contained no upload authorization"}
	}
	entry := response.GetBucketWriteAuth4[0]
	if entry.Error != "" {
		var alreadyExists error
		if strings.Contains(strings.ToLower(entry.Error), "already exist") {
			alreadyExists = ErrAlreadyExists
		}
		return AuthData{}, &Error{operation: "GetBucketWriteAuth4", message: "upload authorization was rejected", err: alreadyExists}
	}
	if entry.AuthData == nil {
		return AuthData{}, &Error{operation: "GetBucketWriteAuth4", message: "response contained no AuthData"}
	}
	return *entry.AuthData, nil
}

// Upload sends one multipart request to GCS. It does not retry because a lost
// response cannot distinguish a failed upload from one that reached storage.
func (c *Client) Upload(ctx context.Context, auth AuthData, name, checksum string, size int64, contentType string, reader io.Reader) error {
	if reader == nil {
		return errors.New("degoo upload: nil content reader")
	}
	if size < 0 {
		return fmt.Errorf("degoo upload: invalid content size %d", size)
	}
	if err := validateUploadBaseURL(auth.BaseURL); err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	extension := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	if extension == "" {
		extension = "bin"
	}
	fields := url.Values{}
	if auth.AccessKey != nil {
		fields.Set(auth.AccessKey.Key, auth.AccessKey.Value)
	}
	fields.Set("policy", auth.PolicyBase64)
	fields.Set("signature", auth.Signature)
	fields.Set("key", fmt.Sprintf("%s%s/%s.%s", auth.KeyPrefix, extension, checksum, extension))
	fields.Set("Content-Type", contentType)
	if auth.ACL != "" {
		fields.Set("acl", auth.ACL)
	}
	for _, value := range auth.AdditionalBody {
		fields.Set(value.Key, value.Value)
	}

	contentLength := size
	storage := rest.NewClient(c.http).SetErrorHandler(safeHTTPError)
	resp, err := storage.CallJSON(ctx, &rest.Opts{
		Method:               http.MethodPost,
		RootURL:              auth.BaseURL,
		Body:                 reader,
		ContentLength:        &contentLength,
		NoResponse:           true,
		MultipartParams:      fields,
		MultipartContentName: "file",
		MultipartFileName:    name,
		MultipartContentType: contentType,
	}, nil, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return &Error{operation: "GCS upload", statusCode: status, message: "storage request failed", err: err}
	}
	if resp == nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent) {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return &Error{operation: "GCS upload", statusCode: status, message: fmt.Sprintf("unexpected HTTP status %d", status)}
	}
	return nil
}

func mutationAcknowledgement(operation string, raw json.RawMessage) (string, error) {
	switch strings.TrimSpace(string(raw)) {
	case "true":
		return "", nil
	case "", "null", "false":
		return "", &Error{operation: operation, message: "mutation was not acknowledged"}
	}
	var result string
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", &Error{operation: operation, message: "response contained an invalid mutation acknowledgement"}
	}
	return result, nil
}

// Register creates the Degoo metadata record after its bytes are uploaded.
// A failure is returned as-is because the caller must retain pending data until
// it can determine whether registration succeeded. It returns legacy string
// responses and an empty string for a boolean success acknowledgement.
func (c *Client) Register(ctx context.Context, parentID, name, checksum string, size int64, created time.Time) (string, error) {
	fileInfo := map[string]string{
		"Name":         name,
		"ParentID":     parentID,
		"Size":         strconv.FormatInt(size, 10),
		"Checksum":     checksum,
		"CreationTime": strconv.FormatInt(created.UnixMilli(), 10),
	}
	var response struct {
		SetUploadFile3 json.RawMessage `json:"setUploadFile3"`
	}
	if err := c.graphql(ctx, "SetUploadFile3", registerMutation, map[string]any{"FileInfos": []map[string]string{fileInfo}}, &response, true); err != nil {
		return "", err
	}
	return mutationAcknowledgement("SetUploadFile3", response.SetUploadFile3)
}

// Mkdir creates a Degoo folder placeholder and returns a string provider response.
// A boolean success acknowledgement returns an empty string.
func (c *Client) Mkdir(ctx context.Context, parentID, name string) (string, error) {
	fileInfo := map[string]string{
		"Name":         name,
		"ParentID":     parentID,
		"Size":         "0",
		"Checksum":     "",
		"CreationTime": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	var response struct {
		SetUploadFile3 json.RawMessage `json:"setUploadFile3"`
	}
	if err := c.graphql(ctx, "SetUploadFile3", registerMutation, map[string]any{"FileInfos": []map[string]string{fileInfo}}, &response, true); err != nil {
		return "", err
	}
	return mutationAcknowledgement("SetUploadFile3", response.SetUploadFile3)
}

// Rename changes the name of one Degoo item.
func (c *Client) Rename(ctx context.Context, id, newName string) error {
	var response struct {
		SetRenameFile json.RawMessage `json:"setRenameFile"`
	}
	if err := c.graphql(ctx, "SetRenameFile", renameMutation, map[string]any{
		"FileRenames": []map[string]string{{"ID": id, "NewName": newName}},
	}, &response, true); err != nil {
		return err
	}
	_, err := mutationAcknowledgement("SetRenameFile", response.SetRenameFile)
	return err
}

// Move moves one Degoo item under a new parent.
func (c *Client) Move(ctx context.Context, id, newParent string) error {
	var response struct {
		SetMoveFile json.RawMessage `json:"setMoveFile"`
	}
	if err := c.graphql(ctx, "SetMoveFile", moveMutation, map[string]any{
		"Copy":        false,
		"NewParentID": newParent,
		"FileIDs":     []string{id},
	}, &response, true); err != nil {
		return err
	}
	_, err := mutationAcknowledgement("SetMoveFile", response.SetMoveFile)
	return err
}

// Delete recycles an active item when inRecycleBin is false. When true, the
// item must already be in the recycle bin and is permanently deleted.
func (c *Client) Delete(ctx context.Context, id string, inRecycleBin bool) error {
	var response struct {
		SetDeleteFile5 json.RawMessage `json:"setDeleteFile5"`
	}
	if err := c.graphql(ctx, "SetDeleteFile5", deleteMutation, map[string]any{
		"IsInRecycleBin": inRecycleBin,
		"IDs":            []map[string]string{{"FileID": id}},
	}, &response, true); err != nil {
		return err
	}
	_, err := mutationAcknowledgement("SetDeleteFile5", response.SetDeleteFile5)
	return err
}

// Download returns an open response body after validating a full or ranged GET.
// A range request must receive HTTP 206 with a matching Content-Range header;
// an un-ranged request must receive HTTP 200. The caller closes the body.
func (c *Client) Download(ctx context.Context, downloadURL, rangeHeader string) (*http.Response, error) {
	var rangeOption *fs.RangeOption
	if rangeHeader != "" {
		var err error
		rangeOption, err = fs.ParseRangeOption(rangeHeader)
		if err != nil {
			return nil, fmt.Errorf("invalid requested range %q: %w", rangeHeader, err)
		}
		if (rangeOption.Start < 0 && rangeOption.End < 0) || (rangeOption.Start >= 0 && rangeOption.End >= 0 && rangeOption.End < rangeOption.Start) {
			return nil, fmt.Errorf("invalid requested range %q", rangeHeader)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, &Error{operation: "download", message: "invalid download request", err: err}
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{operation: "download", message: "HTTP request failed", err: err}
	}
	if rangeHeader == "" {
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, &Error{operation: "download", statusCode: resp.StatusCode, message: fmt.Sprintf("unexpected HTTP status %d for full download", resp.StatusCode)}
		}
		return resp, nil
	}
	if resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, &Error{operation: "download", statusCode: resp.StatusCode, message: fmt.Sprintf("unexpected HTTP status %d for range request", resp.StatusCode)}
	}
	if err := rest.CheckContentRange(resp, []fs.OpenOption{rangeOption}, -1); err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("invalid ranged download response: %w", err)
	}
	return resp, nil
}
