// Package api implements the Degoo GraphQL and object-storage API used by the backend.
package api

import (
	"bytes"
	"context"
	"crypto/sha1"
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
	"path"
	"path/filepath"
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
const defaultRateLimitCooldown = time.Minute
const rateLimitLockTimeout = 5 * time.Second
const metadataCacheTTL = 5 * time.Second
const metadataCacheMaxEntries = 32
const metadataListFlightTimeout = 30 * time.Second
const itemParentCacheMaxEntries = 4096
const maxWriteAuthBatch = 30
const maxRegisterBatch = 100

const unixToInternalSeconds = 62135596800
const maxUnixTimeSeconds = int64(math.MaxInt64) - unixToInternalSeconds

// ErrAlreadyExists indicates that Degoo already has the requested content.
var ErrAlreadyExists = errors.New("degoo content already exists")

// ErrNegativeAcknowledgement indicates that Degoo returned false for a mutation.
// The response alone does not establish whether the mutation had side effects.
var ErrNegativeAcknowledgement = errors.New("degoo mutation returned a false acknowledgement")

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

const trashItemFields = `
		ID
		Name
		ParentID
		Category
		Size
		LastModificationTime
		CreationTime
		IsInRecycleBin
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

const getDeletedFilesQuery = `query GetDeletedFiles(
	$Token: String!
	$Limit: Int!
	$Order: Int!
	$NextToken: String
) {
	getDeletedFiles(
		Token: $Token
		Limit: $Limit
		Order: $Order
		NextToken: $NextToken
	) {
		Items {
` + trashItemFields + `		}
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

	refreshMu          sync.Mutex
	rateLimitMu        sync.Mutex
	rateLimitFile      string
	rateLimitTill      time.Time
	metadataMu         sync.Mutex
	metadataGeneration uint64
	metadataCache      map[string]metadataCacheEntry
	metadataParents    map[string]*metadataParentState
	metadataFlights    map[metadataFlightKey]*metadataListCall
	itemParents        map[string]itemParentEntry
	itemParentSequence uint64
	mutationEpoch      uint64

	statsMu sync.Mutex
	stats   RequestStats

	uploadTransfer   transferWindow
	downloadTransfer transferWindow
}

type metadataCacheEntry struct {
	items            []Item
	expires          time.Time
	createdAt        time.Time
	globalGeneration uint64
	parentGeneration uint64
}

type metadataParentState struct {
	generation    uint64
	activeFlights int
}

type metadataFlightKey struct {
	parentID         string
	globalGeneration uint64
	parentGeneration uint64
	fresh            bool
}

type metadataListCall struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	waiters  int
	finished bool
	items    []Item
	err      error
}

type itemParentEntry struct {
	parentID string
	sequence uint64
}

// UploadInfo describes one file for which Degoo should authorize storage upload.
type UploadInfo struct {
	Name     string // Name is the file name Degoo will associate with the upload.
	Checksum string // Checksum is the content checksum sent to Degoo.
	Size     int64  // Size is the content size in bytes.
}

// WriteAuthResult contains one upload authorization or its per-file error.
type WriteAuthResult struct {
	AuthData AuthData // AuthData contains the storage authorization when Err is nil.
	Err      error    // Err reports a per-file authorization failure.
}

// RegistrationInfo describes one file metadata record to register with Degoo.
type RegistrationInfo struct {
	ParentID string    // ParentID is the destination directory ID.
	Name     string    // Name is the remote file name.
	Checksum string    // Checksum is the content checksum recorded by Degoo.
	Size     int64     // Size is the stored object size in bytes.
	Created  time.Time // Created is the remote creation time.
}

// RequestStats is a concurrency-safe snapshot of API requests and metadata caching.
// It contains operation names and counts only, without request data or identifiers.
type RequestStats struct {
	MetadataCacheHits             uint64            `json:"metadata_cache_hits"`             // MetadataCacheHits counts directory snapshots served from cache.
	MetadataCacheMisses           uint64            `json:"metadata_cache_misses"`           // MetadataCacheMisses counts directory lookups that missed cache.
	MetadataCacheCoalesced        uint64            `json:"metadata_cache_coalesced"`        // MetadataCacheCoalesced counts callers sharing an in-flight listing.
	MetadataPages                 uint64            `json:"metadata_pages"`                  // MetadataPages counts listing HTTP attempts.
	MetadataMutationInvalidations uint64            `json:"metadata_mutation_invalidations"` // MetadataMutationInvalidations counts API mutations that invalidate metadata.
	MetadataParentInvalidations   uint64            `json:"metadata_parent_invalidations"`   // MetadataParentInvalidations counts parent-scoped invalidation events.
	MetadataGlobalInvalidations   uint64            `json:"metadata_global_invalidations"`   // MetadataGlobalInvalidations counts full-cache invalidation events.
	HTTPRequests                  uint64            `json:"http_requests"`                   // HTTPRequests counts GraphQL and object-storage HTTP attempts.
	ReadHTTPRequests              uint64            `json:"read_http_requests"`              // ReadHTTPRequests counts API and storage read attempts.
	WriteHTTPRequests             uint64            `json:"write_http_requests"`             // WriteHTTPRequests counts API and storage write attempts.
	RequestsByOperation           map[string]uint64 `json:"requests_by_operation"`           // RequestsByOperation counts attempts by fixed operation name.
}

type rateLimitFileState struct {
	Version   int   `json:"version"`
	UntilUnix int64 `json:"until_unix"`
}

// RateLimitError reports that Degoo has asked the client to stop making
// requests until Until. It unwraps to the sanitized API error when available.
type RateLimitError struct {
	Until      time.Time
	operation  string
	err        error
	persistErr error
}

// Error returns a sanitized rate-limit deadline without exposing response data.
func (err *RateLimitError) Error() string {
	if err == nil {
		return "degoo API rate limit is active"
	}
	message := "degoo API rate limit is active until " + err.Until.UTC().Format(time.RFC3339)
	if err.operation != "" {
		message += " (operation " + err.operation + ")"
	}
	if err.persistErr != nil {
		message += "; local cooldown could not be persisted"
	}
	return message
}

// Unwrap returns the sanitized API error that started the cooldown.
func (err *RateLimitError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.err
}

// RetryAfter returns the time when the rate limit expires.
func (err *RateLimitError) RetryAfter() time.Time {
	if err == nil {
		return time.Time{}
	}
	return err.Until
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

// SetRateLimitFile loads and persists the provider cooldown in an owner-private
// sidecar. A missing file is accepted; malformed or unsafe files are rejected.
func (c *Client) SetRateLimitFile(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("degoo rate limit file path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve Degoo rate limit sidecar path: %w", err)
	}
	lockCtx, cancel := context.WithTimeout(context.Background(), rateLimitLockTimeout)
	defer cancel()
	err = auth.WithPrivateFileLock(lockCtx, absolute, func() error {
		savedUntil, err := readRateLimitFile(absolute)
		if err != nil {
			return fmt.Errorf("could not read private Degoo rate limit sidecar: %w", err)
		}

		c.rateLimitMu.Lock()
		defer c.rateLimitMu.Unlock()
		if c.rateLimitFile != "" && c.rateLimitFile != absolute {
			return errors.New("degoo rate limit sidecar is already configured")
		}
		previousPath := c.rateLimitFile
		c.rateLimitFile = absolute
		if err := c.mergeRateLimitFileLocked(savedUntil); err != nil {
			c.rateLimitFile = previousPath
			return fmt.Errorf("could not persist private Degoo rate limit sidecar: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("coordinate private Degoo rate limit sidecar: %w", err)
	}
	return nil
}

func readRateLimitFile(path string) (time.Time, error) {
	data, err := auth.ReadPrivateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	until, err := decodeRateLimitFile(data)
	if err != nil {
		return time.Time{}, fmt.Errorf("degoo rate limit sidecar is invalid: %w", err)
	}
	return until, nil
}

func decodeRateLimitFile(data []byte) (time.Time, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state rateLimitFileState
	if err := decoder.Decode(&state); err != nil {
		return time.Time{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return time.Time{}, errors.New("unexpected trailing data")
	}
	if state.Version != 1 || state.UntilUnix <= 0 || state.UntilUnix > maxUnixTimeSeconds {
		return time.Time{}, errors.New("unsupported version or invalid deadline")
	}
	return time.Unix(state.UntilUnix, 0).UTC(), nil
}

// mergeRateLimitFileLocked keeps the in-memory and persisted deadlines monotonic.
// The caller holds both the sidecar file lock and rateLimitMu.
func (c *Client) mergeRateLimitFileLocked(savedUntil time.Time) error {
	if savedUntil.After(c.rateLimitTill) {
		c.rateLimitTill = savedUntil
	}
	if c.rateLimitFile == "" || c.rateLimitTill.IsZero() || !time.Now().Before(c.rateLimitTill) || !c.rateLimitTill.After(savedUntil) {
		return nil
	}
	untilUnix := c.rateLimitTill.UTC().Unix()
	if c.rateLimitTill.Nanosecond() != 0 && untilUnix < maxUnixTimeSeconds {
		untilUnix++
	}
	if untilUnix > maxUnixTimeSeconds {
		untilUnix = maxUnixTimeSeconds
	}
	data, err := json.Marshal(rateLimitFileState{Version: 1, UntilUnix: untilUnix})
	if err != nil {
		return errors.New("could not encode Degoo rate limit sidecar")
	}
	return auth.WritePrivateFile(c.rateLimitFile, append(data, '\n'))
}

func (c *Client) checkRateLimit(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.rateLimitMu.Lock()
	path := c.rateLimitFile
	until := c.rateLimitTill
	c.rateLimitMu.Unlock()
	var persistErr error
	if path != "" {
		lockCtx, cancel := context.WithTimeout(ctx, rateLimitLockTimeout)
		defer cancel()
		err := auth.WithPrivateFileLock(lockCtx, path, func() error {
			savedUntil, err := readRateLimitFile(path)
			if err != nil {
				return fmt.Errorf("could not read private Degoo rate limit sidecar: %w", err)
			}
			c.rateLimitMu.Lock()
			defer c.rateLimitMu.Unlock()
			if err := c.mergeRateLimitFileLocked(savedUntil); err != nil {
				persistErr = fmt.Errorf("could not persist private Degoo rate limit sidecar: %w", err)
			}
			until = c.rateLimitTill
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.rateLimitMu.Lock()
			if c.rateLimitTill.After(until) {
				until = c.rateLimitTill
			}
			c.rateLimitMu.Unlock()
			if !until.IsZero() && time.Now().Before(until) {
				return &RateLimitError{Until: until, operation: operation, persistErr: err}
			}
			return fmt.Errorf("coordinate private Degoo rate limit sidecar: %w", err)
		}
	}
	if until.IsZero() || !time.Now().Before(until) {
		if persistErr != nil {
			return persistErr
		}
		return nil
	}
	return &RateLimitError{Until: until, operation: operation, persistErr: persistErr}
}

// ClearMetadataCache discards cached directory listings and invalidates
// results from listing requests that were already in flight.
func (c *Client) ClearMetadataCache() {
	c.metadataMu.Lock()
	c.clearMetadataCacheLocked()
	c.metadataMu.Unlock()
}

func (c *Client) clearMetadataCacheLocked() {
	c.metadataGeneration++
	c.metadataCache = nil
	for parentID, state := range c.metadataParents {
		if state.activeFlights == 0 {
			delete(c.metadataParents, parentID)
		}
	}
	c.itemParents = nil
	c.itemParentSequence = 0
	c.statsMu.Lock()
	c.stats.MetadataGlobalInvalidations++
	c.statsMu.Unlock()
}

func (c *Client) invalidateMetadataParents(parents []string, known bool) {
	if !known || len(parents) == 0 {
		c.metadataMu.Lock()
		c.clearMetadataCacheLocked()
		c.metadataMu.Unlock()
		return
	}
	unique := make(map[string]struct{}, len(parents))
	for _, parentID := range parents {
		if parentID == "" {
			c.metadataMu.Lock()
			c.clearMetadataCacheLocked()
			c.metadataMu.Unlock()
			return
		}
		unique[parentID] = struct{}{}
	}
	c.metadataMu.Lock()
	for parentID := range unique {
		delete(c.metadataCache, parentID)
		if state := c.metadataParents[parentID]; state != nil {
			state.generation++
			if state.activeFlights == 0 {
				delete(c.metadataParents, parentID)
			}
		}
	}
	c.metadataMu.Unlock()
	c.statsMu.Lock()
	c.stats.MetadataParentInvalidations += uint64(len(unique))
	c.statsMu.Unlock()
}

func (c *Client) recordMutationInvalidation() {
	c.statsMu.Lock()
	c.stats.MetadataMutationInvalidations++
	c.statsMu.Unlock()
}

func (c *Client) recordHTTPRequest(operation string, write bool) {
	c.statsMu.Lock()
	if c.stats.RequestsByOperation == nil {
		c.stats.RequestsByOperation = make(map[string]uint64)
	}
	c.stats.HTTPRequests++
	c.stats.RequestsByOperation[operation]++
	if write {
		c.stats.WriteHTTPRequests++
	} else {
		c.stats.ReadHTTPRequests++
	}
	if operation == "GetFileChildren5" {
		c.stats.MetadataPages++
	}
	c.statsMu.Unlock()
}

func (c *Client) recordMetadataLookup(hit, coalesced bool) {
	c.statsMu.Lock()
	if hit {
		c.stats.MetadataCacheHits++
	} else {
		c.stats.MetadataCacheMisses++
	}
	if coalesced {
		c.stats.MetadataCacheCoalesced++
	}
	c.statsMu.Unlock()
}

func (c *Client) recordMetadataCoalesced() {
	c.statsMu.Lock()
	c.stats.MetadataCacheCoalesced++
	c.statsMu.Unlock()
}

// RequestStats returns a safe snapshot of operation counts and metadata cache activity.
func (c *Client) RequestStats() RequestStats {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	stats := c.stats
	stats.RequestsByOperation = make(map[string]uint64, len(c.stats.RequestsByOperation))
	for operation, count := range c.stats.RequestsByOperation {
		stats.RequestsByOperation[operation] = count
	}
	return stats
}

// TransferStats returns recent transfer rates and cumulative storage payload bytes.
func (c *Client) TransferStats() TransferStats {
	uploadBytes, uploadRate, activeUploads := c.uploadTransfer.snapshot()
	downloadBytes, downloadRate, activeDownloads := c.downloadTransfer.snapshot()
	return TransferStats{
		UploadBytes:            uploadBytes,
		UploadBytesPerSecond:   uploadRate,
		ActiveUploads:          activeUploads,
		DownloadBytes:          downloadBytes,
		DownloadBytesPerSecond: downloadRate,
		ActiveDownloads:        activeDownloads,
	}
}

func cloneItems(items []Item) []Item {
	copyItems := make([]Item, len(items))
	copy(copyItems, items)
	return copyItems
}

func (c *Client) parentStateLocked(parentID string) *metadataParentState {
	if c.metadataParents == nil {
		c.metadataParents = make(map[string]*metadataParentState)
	}
	state := c.metadataParents[parentID]
	if state == nil {
		state = &metadataParentState{}
		c.metadataParents[parentID] = state
	}
	return state
}

func (c *Client) parentGenerationLocked(parentID string) uint64 {
	if state := c.metadataParents[parentID]; state != nil {
		return state.generation
	}
	return 0
}

func (c *Client) invalidateParentLocked(parentID string) {
	delete(c.metadataCache, parentID)
	c.parentStateLocked(parentID).generation++
}

func (c *Client) newMetadataListCallLocked(ctx context.Context, key metadataFlightKey) *metadataListCall {
	if c.metadataFlights == nil {
		c.metadataFlights = make(map[metadataFlightKey]*metadataListCall)
	}
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), metadataListFlightTimeout)
	call := &metadataListCall{ctx: callCtx, cancel: cancel, done: make(chan struct{}), waiters: 1}
	c.metadataFlights[key] = call
	c.parentStateLocked(key.parentID).activeFlights++
	return call
}

func (c *Client) cleanupParentStateLocked(parentID string) {
	state := c.metadataParents[parentID]
	if state == nil || state.activeFlights != 0 {
		return
	}
	if _, exists := c.metadataCache[parentID]; !exists {
		delete(c.metadataParents, parentID)
	}
}

func (c *Client) pruneMetadataCacheLocked(now time.Time) {
	for parentID, entry := range c.metadataCache {
		if !now.Before(entry.expires) {
			delete(c.metadataCache, parentID)
			c.cleanupParentStateLocked(parentID)
		}
	}
}

func (c *Client) cachedListDirLocked(parentID string, globalGeneration, parentGeneration uint64) ([]Item, bool) {
	now := time.Now()
	c.pruneMetadataCacheLocked(now)
	entry, ok := c.metadataCache[parentID]
	if !ok || entry.globalGeneration != globalGeneration || entry.parentGeneration != parentGeneration || !now.Before(entry.expires) {
		return nil, false
	}
	return cloneItems(entry.items), true
}

func (c *Client) cacheListDirLocked(parentID string, items []Item, globalGeneration, parentGeneration uint64) {
	if globalGeneration != c.metadataGeneration || parentGeneration != c.parentGenerationLocked(parentID) {
		return
	}
	now := time.Now()
	if c.metadataCache == nil {
		c.metadataCache = make(map[string]metadataCacheEntry)
	}
	c.pruneMetadataCacheLocked(now)
	if _, exists := c.metadataCache[parentID]; !exists && len(c.metadataCache) >= metadataCacheMaxEntries {
		oldestKey := ""
		var oldestTime time.Time
		haveOldest := false
		for key, entry := range c.metadataCache {
			if !haveOldest || entry.createdAt.Before(oldestTime) {
				oldestKey = key
				oldestTime = entry.createdAt
				haveOldest = true
			}
		}
		if haveOldest {
			delete(c.metadataCache, oldestKey)
			c.cleanupParentStateLocked(oldestKey)
		}
	}
	state := c.parentStateLocked(parentID)
	state.generation = parentGeneration
	c.metadataCache[parentID] = metadataCacheEntry{
		items:            cloneItems(items),
		expires:          now.Add(metadataCacheTTL),
		createdAt:        now,
		globalGeneration: globalGeneration,
		parentGeneration: parentGeneration,
	}
}

func (c *Client) rememberItemsLocked(items []Item) {
	for _, item := range items {
		c.rememberItemParentLocked(item)
	}
}

func (c *Client) rememberItemParentLocked(item Item) {
	if item.ID == "" || item.ParentID == "" {
		return
	}
	if c.itemParents == nil {
		c.itemParents = make(map[string]itemParentEntry)
	}
	c.itemParentSequence++
	if _, exists := c.itemParents[item.ID]; !exists && len(c.itemParents) >= itemParentCacheMaxEntries {
		oldestID := ""
		oldestSequence := uint64(0)
		for id, entry := range c.itemParents {
			if oldestID == "" || entry.sequence < oldestSequence {
				oldestID = id
				oldestSequence = entry.sequence
			}
		}
		delete(c.itemParents, oldestID)
	}
	c.itemParents[item.ID] = itemParentEntry{parentID: item.ParentID, sequence: c.itemParentSequence}
}

func (c *Client) rememberItemParent(item Item) {
	c.metadataMu.Lock()
	c.rememberItemParentLocked(item)
	c.metadataMu.Unlock()
}

func (c *Client) mutationEpochSnapshot() uint64 {
	c.metadataMu.Lock()
	epoch := c.mutationEpoch
	c.metadataMu.Unlock()
	return epoch
}

func (c *Client) rememberItemParentAtMutationEpoch(item Item, epoch uint64) {
	c.metadataMu.Lock()
	if c.mutationEpoch == epoch {
		c.rememberItemParentLocked(item)
	}
	c.metadataMu.Unlock()
}

func (c *Client) rememberItemsAtMutationEpoch(items []Item, epoch uint64) {
	c.metadataMu.Lock()
	if c.mutationEpoch == epoch {
		c.rememberItemsLocked(items)
	}
	c.metadataMu.Unlock()
}

func (c *Client) advanceMutationEpoch() {
	c.metadataMu.Lock()
	c.mutationEpoch++
	c.metadataMu.Unlock()
}

func (c *Client) forgetItemParents(ids []string) {
	c.metadataMu.Lock()
	for _, id := range ids {
		delete(c.itemParents, id)
	}
	c.metadataMu.Unlock()
}

func (c *Client) parentsForItems(ids []string) ([]string, bool) {
	c.metadataMu.Lock()
	defer c.metadataMu.Unlock()
	parents := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		entry, ok := c.itemParents[id]
		if !ok || entry.parentID == "" {
			return nil, false
		}
		if _, exists := seen[entry.parentID]; !exists {
			parents = append(parents, entry.parentID)
			seen[entry.parentID] = struct{}{}
		}
	}
	return parents, len(ids) != 0
}

func (c *Client) rateLimited(operation string, resp *http.Response, cause error) error {
	now := time.Now().UTC()
	var until time.Time
	var ok bool
	if resp != nil {
		until, ok = retryAfterUntil(resp.Header.Get("Retry-After"), now)
	}
	if !ok {
		until = now.Add(defaultRateLimitCooldown)
	}

	c.rateLimitMu.Lock()
	previous := c.rateLimitTill
	if until.After(c.rateLimitTill) {
		c.rateLimitTill = until
	}
	path := c.rateLimitFile
	c.rateLimitMu.Unlock()
	var persistErr error
	if path != "" {
		lockCtx, cancel := context.WithTimeout(context.Background(), rateLimitLockTimeout)
		persistErr = auth.WithPrivateFileLock(lockCtx, path, func() error {
			savedUntil, err := readRateLimitFile(path)
			if err != nil {
				return fmt.Errorf("could not read private Degoo rate limit sidecar: %w", err)
			}
			c.rateLimitMu.Lock()
			defer c.rateLimitMu.Unlock()
			if until.After(c.rateLimitTill) {
				c.rateLimitTill = until
			}
			if savedUntil.After(c.rateLimitTill) {
				c.rateLimitTill = savedUntil
			}
			if err := c.mergeRateLimitFileLocked(savedUntil); err != nil {
				return fmt.Errorf("could not persist private Degoo rate limit sidecar: %w", err)
			}
			until = c.rateLimitTill
			return nil
		})
		cancel()
	}
	c.rateLimitMu.Lock()
	until = c.rateLimitTill
	c.rateLimitMu.Unlock()
	if until.After(previous) {
		fs.Logf(nil, "degoo API rate limited operation=%s retry_utc=%s", operation, until.UTC().Format(time.RFC3339))
	}
	return &RateLimitError{Until: until, operation: operation, err: cause, persistErr: persistErr}
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
	// IsInRecycleBin reports whether the item is in Degoo's recycle bin.
	IsInRecycleBin bool
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
		IsInRecycleBin       bool            `json:"IsInRecycleBin"`
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
		IsInRecycleBin:       wire.IsInRecycleBin,
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
	if len(err.graphQLErrors) != 0 {
		classes := make([]string, 0, len(err.graphQLErrors))
		seen := make(map[string]bool)
		for _, gqlErr := range err.graphQLErrors {
			if !seen[gqlErr.typ] {
				classes = append(classes, gqlErr.typ)
				seen[gqlErr.typ] = true
			}
		}
		message += " (" + strings.Join(classes, ", ") + ")"
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
	case strings.Contains(classification, "validation"):
		typ = "ValidationException"
	case strings.Contains(classification, "notfound"), strings.Contains(classification, "not found"):
		typ = "NotFoundException"
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
	var rateLimitErr *RateLimitError
	if errors.As(err, &rateLimitErr) {
		return http.StatusTooManyRequests
	}
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
	var rateLimitErr *RateLimitError
	if errors.As(err, &rateLimitErr) {
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

func retryAfterUntil(header string, now time.Time) (time.Time, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return time.Time{}, false
	}
	if decimalHeader(header) {
		seconds, err := strconv.ParseUint(header, 10, 64)
		if err != nil {
			return maxRepresentableTime(), true
		}
		if seconds == 0 {
			return time.Time{}, false
		}
		return addSecondsSaturated(now, seconds), true
	}
	when, err := http.ParseTime(header)
	if err != nil || !when.After(now) {
		return time.Time{}, false
	}
	return when.UTC(), true
}

func decimalHeader(header string) bool {
	for _, char := range header {
		if char < '0' || char > '9' {
			return false
		}
	}
	return header != ""
}

func addSecondsSaturated(now time.Time, seconds uint64) time.Time {
	now = now.UTC()
	base := now.Unix()
	if base < 0 || base >= maxUnixTimeSeconds || seconds > uint64(maxUnixTimeSeconds-base) {
		return maxRepresentableTime()
	}
	return time.Unix(base+int64(seconds), int64(now.Nanosecond())).UTC()
}

func maxRepresentableTime() time.Time {
	return time.Unix(maxUnixTimeSeconds, 999999999).UTC()
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
	if err := c.checkRateLimit(ctx, operation); err != nil {
		return err
	}
	token, err := c.tokenForRequest(ctx)
	if err != nil {
		return &Error{operation: operation, message: "cannot obtain access token", err: err}
	}

	for attempt := 0; attempt < 2; attempt++ {
		if err := c.checkRateLimit(ctx, operation); err != nil {
			return err
		}
		vars := make(map[string]any, len(variables)+1)
		for key, value := range variables {
			vars[key] = value
		}
		vars["Token"] = token
		request := map[string]any{"query": query, "operationName": operation, "variables": vars}
		var envelope graphQLEnvelope
		c.recordHTTPRequest(operation, mutation || operation == "GetBucketWriteAuth4")
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
			if status == http.StatusTooManyRequests {
				return c.rateLimited(operation, resp, apiErr)
			}
			if attempt == 0 && !mutation && isUnauthorized(status, nil) {
				if err := c.checkRateLimit(ctx, operation); err != nil {
					return err
				}
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
			if attempt == 0 && !mutation && canRefreshGraphQL(false, envelope.Data, gqlErrors) {
				if err := c.checkRateLimit(ctx, operation); err != nil {
					return err
				}
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

// ListDir returns a short-lived cached directory snapshot when available and
// otherwise loads every item under parentID, requesting at most 1,000 items per
// page. It rejects repeated item IDs and repeated pagination cursors.
func (c *Client) ListDir(ctx context.Context, parentID string) ([]Item, error) {
	return c.listDir(ctx, parentID, false)
}

// ListDirFresh loads every item under parentID without reading or storing a
// cached snapshot. It invalidates only that directory before and after loading.
func (c *Client) ListDirFresh(ctx context.Context, parentID string) ([]Item, error) {
	return c.listDir(ctx, parentID, true)
}

func (c *Client) listDir(ctx context.Context, parentID string, fresh bool) ([]Item, error) {
	if err := c.checkRateLimit(ctx, "GetFileChildren5"); err != nil {
		return nil, err
	}

	var (
		call      *metadataListCall
		key       metadataFlightKey
		startCall bool
		cacheHit  bool
		items     []Item
	)
	c.metadataMu.Lock()
	c.pruneMetadataCacheLocked(time.Now())
	globalGeneration := c.metadataGeneration
	parentGeneration := c.parentGenerationLocked(parentID)
	if fresh {
		key = metadataFlightKey{parentID: parentID, globalGeneration: globalGeneration, parentGeneration: parentGeneration, fresh: true}
		if existing := c.metadataFlights[key]; existing != nil && !existing.finished {
			existing.waiters++
			call = existing
		} else {
			c.invalidateParentLocked(parentID)
			parentGeneration = c.parentGenerationLocked(parentID)
			key = metadataFlightKey{parentID: parentID, globalGeneration: globalGeneration, parentGeneration: parentGeneration, fresh: true}
			call = c.newMetadataListCallLocked(ctx, key)
			startCall = true
		}
	} else {
		if cached, ok := c.cachedListDirLocked(parentID, globalGeneration, parentGeneration); ok {
			items = cached
			cacheHit = true
		} else {
			key = metadataFlightKey{parentID: parentID, globalGeneration: globalGeneration, parentGeneration: parentGeneration}
			if existing := c.metadataFlights[key]; existing != nil && !existing.finished {
				existing.waiters++
				call = existing
			} else {
				call = c.newMetadataListCallLocked(ctx, key)
				startCall = true
			}
		}
	}
	c.metadataMu.Unlock()

	if fresh {
		if startCall {
			c.recordParentInvalidation()
		} else {
			c.recordMetadataCoalesced()
		}
	} else if cacheHit {
		c.recordMetadataLookup(true, false)
		return items, nil
	} else {
		c.recordMetadataLookup(false, !startCall)
	}
	if startCall {
		go c.runMetadataListCall(key, call)
	}
	return c.waitForMetadataList(ctx, key, call)
}

func (c *Client) recordParentInvalidation() {
	c.statsMu.Lock()
	c.stats.MetadataParentInvalidations++
	c.statsMu.Unlock()
}

func (c *Client) waitForMetadataList(ctx context.Context, key metadataFlightKey, call *metadataListCall) ([]Item, error) {
	select {
	case <-ctx.Done():
		c.removeMetadataListWaiter(key, call)
		return nil, ctx.Err()
	case <-call.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return cloneItems(call.items), call.err
	}
}

func (c *Client) removeMetadataListWaiter(key metadataFlightKey, call *metadataListCall) {
	c.metadataMu.Lock()
	if !call.finished {
		call.waiters--
		if call.waiters == 0 {
			if c.metadataFlights[key] == call {
				delete(c.metadataFlights, key)
			}
			call.cancel()
		}
	}
	c.metadataMu.Unlock()
}

func (c *Client) runMetadataListCall(key metadataFlightKey, call *metadataListCall) {
	items, err := c.listDirUncached(call.ctx, key.parentID)
	c.metadataMu.Lock()
	if err == nil && call.ctx.Err() == nil && key.globalGeneration == c.metadataGeneration && key.parentGeneration == c.parentGenerationLocked(key.parentID) {
		if key.fresh {
			c.rememberItemsLocked(items)
		} else {
			c.cacheListDirLocked(key.parentID, items, key.globalGeneration, key.parentGeneration)
			c.rememberItemsLocked(items)
		}
	}
	if key.fresh {
		// Fresh reads are always invalidating reads, including failed reads.
		c.invalidateParentLocked(key.parentID)
	}
	if c.metadataFlights[key] == call {
		delete(c.metadataFlights, key)
	}
	if state := c.metadataParents[key.parentID]; state != nil && state.activeFlights > 0 {
		state.activeFlights--
	}
	c.cleanupParentStateLocked(key.parentID)
	call.items = cloneItems(items)
	call.err = err
	call.finished = true
	close(call.done)
	c.metadataMu.Unlock()
	call.cancel()
	if key.fresh {
		c.recordParentInvalidation()
	}
}

func (c *Client) listDirUncached(ctx context.Context, parentID string) ([]Item, error) {
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

func (c *Client) beginMutationInvalidation(parents []string, known bool) func() {
	c.advanceMutationEpoch()
	c.invalidateMetadataParents(parents, known)
	c.recordMutationInvalidation()
	var finishOnce sync.Once
	return func() {
		finishOnce.Do(func() {
			c.advanceMutationEpoch()
			c.invalidateMetadataParents(parents, known)
		})
	}
}

// ListTrash returns items in the account-wide recycle bin. A limit of zero or
// less requests every page; a positive limit returns at most that many items.
// It rejects items that are not marked as recycled, repeated item IDs, and
// repeated pagination cursors.
func (c *Client) ListTrash(ctx context.Context, limit int) ([]Item, error) {
	if limit < 0 {
		return nil, &Error{operation: "GetDeletedFiles", message: "negative item limit"}
	}
	mutationEpoch := c.mutationEpochSnapshot()
	items := make([]Item, 0)
	seenIDs := make(map[string]struct{})
	seenTokens := make(map[string]struct{})
	var nextToken string
	for {
		pageLimit := maxLimit
		if limit > 0 {
			pageLimit = min(maxLimit, limit-len(items))
		}
		variables := map[string]any{"Limit": pageLimit, "Order": 3}
		if nextToken != "" {
			variables["NextToken"] = nextToken
		}
		var response struct {
			GetDeletedFiles struct {
				Items     []Item `json:"Items"`
				NextToken string `json:"NextToken"`
			} `json:"getDeletedFiles"`
		}
		if err := c.graphql(ctx, "GetDeletedFiles", getDeletedFilesQuery, variables, &response, false); err != nil {
			return nil, err
		}
		for _, item := range response.GetDeletedFiles.Items {
			if item.ID == "" {
				return nil, &Error{operation: "GetDeletedFiles", message: "response contained an item with an empty ID"}
			}
			if !item.IsInRecycleBin {
				return nil, &Error{operation: "GetDeletedFiles", message: fmt.Sprintf("item %q was not marked as in the recycle bin", item.ID)}
			}
			if _, exists := seenIDs[item.ID]; exists {
				return nil, &Error{operation: "GetDeletedFiles", message: fmt.Sprintf("duplicate item ID %q across pages", item.ID)}
			}
			seenIDs[item.ID] = struct{}{}
			items = append(items, item)
		}
		if limit > 0 && len(items) >= limit {
			items = items[:limit]
			c.rememberItemsAtMutationEpoch(items, mutationEpoch)
			return items, nil
		}
		nextToken = response.GetDeletedFiles.NextToken
		if nextToken == "" {
			c.rememberItemsAtMutationEpoch(items, mutationEpoch)
			return items, nil
		}
		if _, exists := seenTokens[nextToken]; exists {
			return nil, &Error{operation: "GetDeletedFiles", message: "response repeated a pagination cursor"}
		}
		seenTokens[nextToken] = struct{}{}
	}
}

// GetItem returns fresh metadata for an item, including its short-lived URL.
func (c *Client) GetItem(ctx context.Context, id string) (Item, error) {
	mutationEpoch := c.mutationEpochSnapshot()
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
	c.rememberItemParentAtMutationEpoch(*response.GetOverlay4, mutationEpoch)
	return *response.GetOverlay4, nil
}

// GetWriteAuthBatch requests storage authorizations for up to 30 files.
func (c *Client) GetWriteAuthBatch(ctx context.Context, parentID string, infos []UploadInfo) ([]WriteAuthResult, error) {
	if len(infos) > maxWriteAuthBatch {
		return nil, &Error{operation: "GetBucketWriteAuth4", message: fmt.Sprintf("batch contains %d uploads; maximum is %d", len(infos), maxWriteAuthBatch)}
	}
	if len(infos) == 0 {
		return []WriteAuthResult{}, nil
	}
	storageUploadInfos := make([]map[string]string, 0, len(infos))
	for _, info := range infos {
		storageUploadInfos = append(storageUploadInfos, map[string]string{
			"FileName": info.Name,
			"Checksum": info.Checksum,
			"Size":     strconv.FormatInt(info.Size, 10),
		})
	}
	variables := map[string]any{
		"ParentID":           parentID,
		"StorageUploadInfos": storageUploadInfos,
	}
	var response struct {
		GetBucketWriteAuth4 []struct {
			AuthData *AuthData `json:"AuthData"`
			Error    string    `json:"Error"`
		} `json:"getBucketWriteAuth4"`
	}
	if err := c.graphql(ctx, "GetBucketWriteAuth4", getWriteAuthQuery, variables, &response, false); err != nil {
		return nil, err
	}
	if len(response.GetBucketWriteAuth4) != len(infos) {
		return nil, &Error{operation: "GetBucketWriteAuth4", message: fmt.Sprintf("response returned %d authorizations for %d uploads", len(response.GetBucketWriteAuth4), len(infos))}
	}
	results := make([]WriteAuthResult, len(infos))
	for index, entry := range response.GetBucketWriteAuth4 {
		if entry.Error != "" {
			var alreadyExists error
			if strings.Contains(strings.ToLower(entry.Error), "already exist") {
				alreadyExists = ErrAlreadyExists
			}
			results[index].Err = &Error{operation: "GetBucketWriteAuth4", message: "upload authorization was rejected", err: alreadyExists}
			continue
		}
		if entry.AuthData == nil {
			results[index].Err = &Error{operation: "GetBucketWriteAuth4", message: "response contained no AuthData"}
			continue
		}
		results[index].AuthData = *entry.AuthData
	}
	return results, nil
}

// GetWriteAuth returns the GCS authorization for one upload.
func (c *Client) GetWriteAuth(ctx context.Context, parentID, name string, size int64, checksum string) (AuthData, error) {
	results, err := c.GetWriteAuthBatch(ctx, parentID, []UploadInfo{{Name: name, Size: size, Checksum: checksum}})
	if err != nil {
		return AuthData{}, err
	}
	if len(results) != 1 {
		return AuthData{}, &Error{operation: "GetBucketWriteAuth4", message: "response contained no upload authorization"}
	}
	return results[0].AuthData, results[0].Err
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
	if err := c.checkRateLimit(ctx, "GCS upload"); err != nil {
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
	reader = &transferReader{reader: reader, stats: &c.uploadTransfer}
	finishUpload := c.uploadTransfer.begin()
	defer finishUpload()
	c.recordHTTPRequest("GCS upload", true)
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
		apiErr := &Error{operation: "GCS upload", statusCode: status, message: "storage request failed", err: err}
		if status == http.StatusTooManyRequests {
			return c.rateLimited("GCS upload", resp, apiErr)
		}
		return apiErr
	}
	if resp == nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent) {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		apiErr := &Error{operation: "GCS upload", statusCode: status, message: fmt.Sprintf("unexpected HTTP status %d", status)}
		if status == http.StatusTooManyRequests {
			return c.rateLimited("GCS upload", resp, apiErr)
		}
		return apiErr
	}
	return nil
}

func mutationAcknowledgement(operation string, raw json.RawMessage) (string, error) {
	switch strings.TrimSpace(string(raw)) {
	case "true":
		return "", nil
	case "":
		return "", &Error{operation: operation, message: "response omitted the mutation acknowledgement; outcome is ambiguous"}
	case "null":
		return "", &Error{operation: operation, message: "mutation acknowledgement was null; outcome is ambiguous"}
	case "false":
		return "", &Error{operation: operation, message: "mutation acknowledgement was false; outcome is ambiguous", err: ErrNegativeAcknowledgement}
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
	return c.RegisterBatch(ctx, []RegistrationInfo{{ParentID: parentID, Name: name, Checksum: checksum, Size: size, Created: created}})
}

// RegisterBatch registers up to 100 uploaded files in one non-replayed mutation.
// An ambiguous response is returned to the caller for reconciliation.
func (c *Client) RegisterBatch(ctx context.Context, infos []RegistrationInfo) (string, error) {
	if len(infos) > maxRegisterBatch {
		return "", &Error{operation: "SetUploadFile3", message: fmt.Sprintf("batch contains %d registrations; maximum is %d", len(infos), maxRegisterBatch)}
	}
	if len(infos) == 0 {
		return "", nil
	}
	fileInfos := make([]map[string]string, 0, len(infos))
	parents := make([]string, 0, len(infos))
	knownParents := true
	for _, info := range infos {
		fileInfos = append(fileInfos, map[string]string{
			"Name":         info.Name,
			"ParentID":     info.ParentID,
			"Size":         strconv.FormatInt(info.Size, 10),
			"Checksum":     info.Checksum,
			"CreationTime": strconv.FormatInt(info.Created.UnixMilli(), 10),
		})
		if info.ParentID == "" {
			knownParents = false
		} else {
			parents = append(parents, info.ParentID)
		}
	}
	finishInvalidation := c.beginMutationInvalidation(parents, knownParents)
	defer finishInvalidation()
	var response struct {
		SetUploadFile3 json.RawMessage `json:"setUploadFile3"`
	}
	if err := c.graphql(ctx, "SetUploadFile3", registerMutation, map[string]any{"FileInfos": fileInfos}, &response, true); err != nil {
		return "", err
	}
	return mutationAcknowledgement("SetUploadFile3", response.SetUploadFile3)
}

// Mkdir creates a Degoo folder placeholder and returns a string provider response.
// A boolean success acknowledgement returns an empty string.
func (c *Client) Mkdir(ctx context.Context, parentID, name string) (string, error) {
	return c.RegisterBatch(ctx, []RegistrationInfo{{ParentID: parentID, Name: name, Created: time.Now()}})
}

// Rename changes the name of one Degoo item.
func (c *Client) Rename(ctx context.Context, id, newName string) error {
	parents, known := c.parentsForItems([]string{id})
	finishInvalidation := c.beginMutationInvalidation(parents, known)
	mutationSucceeded := false
	defer func() {
		finishInvalidation()
		if !mutationSucceeded {
			c.forgetItemParents([]string{id})
		}
	}()
	var response struct {
		SetRenameFile json.RawMessage `json:"setRenameFile"`
	}
	if err := c.graphql(ctx, "SetRenameFile", renameMutation, map[string]any{
		"FileRenames": []map[string]string{{"ID": id, "NewName": newName}},
	}, &response, true); err != nil {
		return err
	}
	_, err := mutationAcknowledgement("SetRenameFile", response.SetRenameFile)
	mutationSucceeded = err == nil
	return err
}

// Move moves one Degoo item under a new parent.
func (c *Client) Move(ctx context.Context, id, newParent string) error {
	parents, known := c.parentsForItems([]string{id})
	if newParent == "" {
		known = false
	} else {
		parents = append(parents, newParent)
	}
	finishInvalidation := c.beginMutationInvalidation(parents, known)
	mutationSucceeded := false
	defer func() {
		finishInvalidation()
		if !mutationSucceeded {
			c.forgetItemParents([]string{id})
		}
	}()
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
	if err != nil {
		return err
	}
	finishInvalidation()
	c.rememberItemParent(Item{ID: id, ParentID: newParent})
	mutationSucceeded = true
	return nil
}

// Delete recycles an active item when inRecycleBin is false. When true, the
// item must already be in the recycle bin and is permanently deleted.
func (c *Client) Delete(ctx context.Context, id string, inRecycleBin bool) error {
	parents, known := c.parentsForItems([]string{id})
	finishInvalidation := c.beginMutationInvalidation(parents, known)
	defer func() {
		finishInvalidation()
		c.forgetItemParents([]string{id})
	}()
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

// DeleteTrashItems permanently deletes items that are already in Degoo's
// recycle bin. An empty input is a successful no-op.
func (c *Client) DeleteTrashItems(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	input := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			return &Error{operation: "SetDeleteFile5", message: "permanent deletion requested an item with an empty ID"}
		}
		input = append(input, map[string]string{"FileID": id})
	}
	parents, known := c.parentsForItems(ids)
	finishInvalidation := c.beginMutationInvalidation(parents, known)
	defer func() {
		finishInvalidation()
		c.forgetItemParents(ids)
	}()
	var response struct {
		SetDeleteFile5 json.RawMessage `json:"setDeleteFile5"`
	}
	if err := c.graphql(ctx, "SetDeleteFile5", deleteMutation, map[string]any{
		"IsInRecycleBin": true,
		"IDs":            input,
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
	if err := c.checkRateLimit(ctx, "download"); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, &Error{operation: "download", message: "invalid download request", err: err}
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	c.recordHTTPRequest("download", false)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{operation: "download", message: "HTTP request failed", err: err}
	}
	if rangeHeader == "" {
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			apiErr := &Error{operation: "download", statusCode: resp.StatusCode, message: fmt.Sprintf("unexpected HTTP status %d for full download", resp.StatusCode)}
			if resp.StatusCode == http.StatusTooManyRequests {
				return nil, c.rateLimited("download", resp, apiErr)
			}
			return nil, apiErr
		}
		c.trackDownloadResponse(resp)
		return resp, nil
	}
	if resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		apiErr := &Error{operation: "download", statusCode: resp.StatusCode, message: fmt.Sprintf("unexpected HTTP status %d for range request", resp.StatusCode)}
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, c.rateLimited("download", resp, apiErr)
		}
		return nil, apiErr
	}
	if err := rest.CheckContentRange(resp, []fs.OpenOption{rangeOption}, -1); err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("invalid ranged download response: %w", err)
	}
	c.trackDownloadResponse(resp)
	return resp, nil
}

func (c *Client) trackDownloadResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	finish := c.downloadTransfer.begin()
	resp.Body = &transferReadCloser{reader: resp.Body, stats: &c.downloadTransfer, finish: finish}
}
