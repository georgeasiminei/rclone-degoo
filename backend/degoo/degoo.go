// Package degoo implements a raw Degoo filesystem for an encrypted vault stack.
package degoo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	stdhash "hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/backend/degoo/auth"
	"github.com/rclone/rclone/backend/overview"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/pacer"
)

const (
	defaultGraphQLURL = "https://production-appsync.degoo.com/graphql"
	defaultRESTURL    = "https://rest-api.degoo.com"
	defaultAPIKey     = "da2-vs6twz5vnjdavpqndtbzg3prra"

	directoryMarkerPrefix  = ".rclone-degoo-dir-marker-"
	retiredDirectoryPrefix = ".rclone-degoo-retired-dir-"
	temporaryPrefix        = ".rclone-degoo-upload-"
	backupPrefix           = ".rclone-degoo-backup-"

	directoryPromotionTimeout   = 3 * time.Minute
	uploadRegistrationTimeout   = 20 * time.Second
	negativeRegistrationTimeout = 2 * time.Second

	metadataLookupTimeout = 10 * time.Second
	metadataRetryDelay    = 100 * time.Millisecond
	metadataMaxAttempts   = 3
	maxPacerSleep         = 30 * time.Second
	downloadURLMaxAge     = 30 * time.Second
	rootProofVersion      = 1
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "degoo",
		Description: "Degoo (experimental raw vault storage)",
		NewFs:       NewFs,
		Overview:    &overview.BackendConfig{Backend: "degoo", Name: "Degoo", IntegrationTests: "not run"},
		Options: []fs.Option{{
			Name:      "auth_file",
			Help:      "Path to the private file holding Degoo account tokens.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:      "credentials_file",
			Help:      "Path to the private encrypted vault configuration holding optional Degoo account credentials.",
			Sensitive: true,
		}, {
			Name:      "root_folder_id",
			Help:      "Degoo folder ID reserved for the encrypted vault; root ID 0 is not allowed.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:     "api_key",
			Help:     "Degoo AppSync routing key. This public key can be overridden if Degoo rotates it.",
			Default:  defaultAPIKey,
			Advanced: true,
		}, {
			Name:     "api_url",
			Help:     "Degoo GraphQL endpoint.",
			Default:  defaultGraphQLURL,
			Advanced: true,
		}, {
			Name:     "rest_url",
			Help:     "Degoo REST API base URL used for token refresh.",
			Default:  defaultRESTURL,
			Advanced: true,
		}, {
			Name:     "skip_upload_verification",
			Help:     "Skip the full download and hash readback before publishing an upload. Registered metadata and the canonical published listing are still checked, but Degoo may store bytes that differ from the upload.",
			Default:  false,
			Advanced: true,
		}},
	})
}

var errMutationAmbiguous = errors.New("degoo mutation state is ambiguous")

// Options defines the configuration for the Degoo backend.
type Options struct {
	AuthFile               string `config:"auth_file"`                // AuthFile is the path to private Degoo token state.
	CredentialsFile        string `config:"credentials_file"`         // CredentialsFile is the path to encrypted account credentials.
	RootFolderID           string `config:"root_folder_id"`           // RootFolderID is the dedicated encrypted vault folder ID.
	APIKey                 string `config:"api_key"`                  // APIKey is the AppSync routing key.
	APIURL                 string `config:"api_url"`                  // APIURL is the GraphQL endpoint.
	RESTURL                string `config:"rest_url"`                 // RESTURL is the authentication endpoint base URL.
	SkipUploadVerification bool   `config:"skip_upload_verification"` // SkipUploadVerification disables full payload readback after upload.
}

// Fs represents a Degoo vault root.
type Fs struct {
	name     string
	root     string
	opt      Options
	api      *api.Client
	dirCache *dircache.DirCache
	features *fs.Features

	placeholderMu sync.RWMutex
	placeholders  map[string]struct{}
	writeMu       sync.Mutex
}

// Object describes a file stored in Degoo.
type Object struct {
	fs       *Fs
	remote   string
	id       string
	parentID string
	name     string
	size     int64
	modTime  time.Time
	url      string
	urlAt    time.Time
}

type rootValidationProof struct {
	Version      int    `json:"version"`
	APIURL       string `json:"api_url"`
	RootFolderID string `json:"root_folder_id"`
	Validated    bool   `json:"validated"`
}

// NewFs constructs a Degoo filesystem rooted under a configured vault folder.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opt.AuthFile) == "" {
		return nil, errors.New("degoo: auth_file is required")
	}
	if strings.TrimSpace(opt.RootFolderID) != opt.RootFolderID || opt.RootFolderID == "" || opt.RootFolderID == "0" {
		return nil, errors.New("degoo: root_folder_id must identify a dedicated vault folder")
	}
	if opt.APIKey == "" {
		opt.APIKey = defaultAPIKey
	}
	if opt.APIURL == "" {
		opt.APIURL = defaultGraphQLURL
	}
	if opt.RESTURL == "" {
		opt.RESTURL = defaultRESTURL
	}
	if err := validateEndpoint(opt.APIURL); err != nil {
		return nil, fmt.Errorf("degoo: invalid api_url: %w", err)
	}

	root, err := normalizeRemote(root)
	if err != nil {
		return nil, fmt.Errorf("degoo: invalid root path: %w", err)
	}

	// Degoo's GraphQL request body contains the access token and signed download
	// URLs are short-lived credentials, so this backend never permits HTTP dumps.
	clientCtx, ci := fs.AddConfig(ctx)
	ci.Dump = 0
	httpClient := fshttp.NewClient(clientCtx)
	authClient, err := auth.NewWithCredentials(httpClient, opt.AuthFile, opt.RESTURL, opt.CredentialsFile)
	if err != nil {
		return nil, err
	}

	f := &Fs{
		name:         name,
		root:         root,
		opt:          *opt,
		api:          api.New(httpClient, authClient, opt.APIURL, opt.APIKey),
		placeholders: make(map[string]struct{}),
	}
	if err := f.api.SetRateLimitFile(opt.AuthFile + ".rate-limit.json"); err != nil {
		return nil, fmt.Errorf("degoo: load provider rate-limit state: %w", err)
	}
	f.dirCache = dircache.New(root, opt.RootFolderID, f)
	f.features = (&fs.Features{
		CaseInsensitive:          false,
		DuplicateFiles:           false,
		CanHaveEmptyDirectories:  true,
		DirModTimeUpdatesOnWrite: true,
		Move:                     f.Move,
		DirMove:                  f.DirMove,
		DirCacheFlush:            f.DirCacheFlush,
		PutStream:                f.PutStream,
	}).Fill(ctx, f)

	rootItem, err := f.getItem(ctx, opt.RootFolderID)
	offlineRoot := false
	if err != nil {
		var rateLimitErr *api.RateLimitError
		if !errors.As(err, &rateLimitErr) || root != "" {
			return nil, fmt.Errorf("degoo: inspect configured vault folder ID %q: %w", opt.RootFolderID, err)
		}
		valid, proofErr := f.hasRootValidationProof()
		if proofErr != nil {
			return nil, fmt.Errorf("degoo: inspect cached vault root validation: %w", proofErr)
		}
		if !valid {
			return nil, fmt.Errorf("degoo: cannot validate configured vault folder during provider rate limit; no matching prior validation proof is available: %w", err)
		}
		offlineRoot = true
		fs.Logf(f, "degoo: using previously validated vault root during provider cooldown until %s", rateLimitErr.Until.UTC().Format(time.RFC3339))
	} else {
		isDir, err := f.itemIsDirectory(ctx, rootItem)
		if err != nil {
			return nil, fmt.Errorf("degoo: inspect configured vault folder children: %w", err)
		}
		if !isDir {
			return nil, errors.New("degoo: root_folder_id does not identify a directory")
		}
		fs.Debugf(f, "degoo: vault folder resolved (ID %q, parent ID %q, category %d)", rootItem.ID, rootItem.ParentID, rootItem.Category)
		if err := f.retireConfiguredRootPlaceholder(ctx, rootItem); err != nil {
			return nil, fmt.Errorf("degoo: reconcile configured vault folder: %w", err)
		}
	}
	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		if root == "" {
			return nil, fmt.Errorf("degoo: resolve vault root: %w", err)
		}
		parentPath, leaf := dircache.SplitPath(root)
		parentCache := dircache.New(parentPath, opt.RootFolderID, f)
		if parentErr := parentCache.FindRoot(ctx, false); parentErr != nil {
			if errors.Is(parentErr, fs.ErrorDirNotFound) {
				if !offlineRoot {
					f.saveRootValidationProof()
				}
				return f, nil
			}
			return nil, fmt.Errorf("degoo: resolve root parent: %w", parentErr)
		}
		parentID, parentErr := parentCache.FindDir(ctx, "", false)
		if parentErr != nil {
			return nil, fmt.Errorf("degoo: resolve root parent ID: %w", parentErr)
		}
		item, isDir, _, lookupErr := f.lookupChild(ctx, parentID, leaf)
		if lookupErr != nil {
			if isNotFoundError(lookupErr) {
				if !offlineRoot {
					f.saveRootValidationProof()
				}
				return f, nil
			}
			return nil, fmt.Errorf("degoo: inspect root path: %w", lookupErr)
		}
		if item == nil {
			if !offlineRoot {
				f.saveRootValidationProof()
			}
			return f, nil
		}
		if isDir {
			return nil, fmt.Errorf("degoo: resolve root path: %w", err)
		}
		f.root = parentPath
		f.dirCache = parentCache
		return f, fs.ErrorIsFile
	}
	if !offlineRoot {
		f.saveRootValidationProof()
	}
	return f, nil
}

func (f *Fs) rootValidationProofPath() string {
	return f.opt.AuthFile + ".root.json"
}

func (f *Fs) hasRootValidationProof() (bool, error) {
	data, err := auth.ReadPrivateFile(f.rootValidationProofPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var proof rootValidationProof
	if err := json.Unmarshal(data, &proof); err != nil {
		return false, nil
	}
	return proof.Version == rootProofVersion && proof.Validated && proof.APIURL == f.opt.APIURL && proof.RootFolderID == f.opt.RootFolderID, nil
}

func (f *Fs) saveRootValidationProof() {
	proof := rootValidationProof{
		Version:      rootProofVersion,
		APIURL:       f.opt.APIURL,
		RootFolderID: f.opt.RootFolderID,
		Validated:    true,
	}
	data, err := json.Marshal(proof)
	if err == nil {
		err = auth.WritePrivateFile(f.rootValidationProofPath(), data)
	}
	if err != nil {
		fs.Errorf(f, "degoo: could not save prior root validation proof: %v", err)
	}
}

// RootFolderID returns the configured root ID for wrapper validation.
func (f *Fs) RootFolderID() string { return f.opt.RootFolderID }

func (f *Fs) sameVault(other *Fs) bool {
	if other == nil || f.opt.RootFolderID != other.opt.RootFolderID || f.opt.APIKey != other.opt.APIKey || f.opt.APIURL != other.opt.APIURL || f.opt.RESTURL != other.opt.RESTURL {
		return false
	}
	left, leftErr := filepath.Abs(f.opt.AuthFile)
	right, rightErr := filepath.Abs(other.opt.AuthFile)
	return leftErr == nil && rightErr == nil && left == right
}

// Name returns the name of the remote.
func (f *Fs) Name() string { return f.name }

// Root returns the root path of the remote.
func (f *Fs) Root() string { return f.root }

// String describes the filesystem.
func (f *Fs) String() string { return "Degoo vault" }

// Precision reports that Degoo does not support setting modification times.
func (f *Fs) Precision() time.Duration { return fs.ModTimeNotSupported }

// Hashes returns no hashes because Degoo's checksum is not a standard hash.
func (f *Fs) Hashes() hash.Set { return hash.NewHashSet() }

// Features returns the optional capabilities of the filesystem.
func (f *Fs) Features() *fs.Features { return f.features }

// FindLeaf resolves a directory child under pathID.
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (string, bool, error) {
	if err := validateLeaf(leaf); err != nil {
		return "", false, err
	}
	item, isDir, ghosts, err := f.lookupChild(ctx, pathID, leaf)
	if err != nil || item == nil {
		return "", false, err
	}
	if !isDir {
		return "", false, fs.ErrorDirNotFound
	}
	if err := f.retirePromotedPlaceholder(ctx, pathID, *item, ghosts); err != nil {
		return "", false, err
	}
	if item.Category == 6 {
		f.rememberPlaceholder(item.ID)
	}
	return item.ID, true, nil
}

// CreateDir creates a directory below pathID and returns its provider ID.
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (string, error) {
	if err := validateLeaf(leaf); err != nil {
		return "", err
	}
	item, isDir, ghosts, err := f.lookupChild(ctx, pathID, leaf)
	if err != nil {
		return "", err
	}
	if item != nil {
		if isDir {
			if err := f.retirePromotedPlaceholder(ctx, pathID, *item, ghosts); err != nil {
				return "", err
			}
			return item.ID, nil
		}
		return "", fs.ErrorDirExists
	}

	_, createErr := f.api.Mkdir(ctx, pathID, leaf)
	resolveTimeout := 20 * time.Second
	if createErr != nil {
		if !api.Retryable(createErr) {
			return "", fmt.Errorf("degoo: create directory placeholder: %w", createErr)
		}
		resolveTimeout = 2 * time.Second
	}
	placeholder, err := f.waitForItem(ctx, pathID, leaf, resolveTimeout)
	if err != nil {
		if createErr != nil {
			return "", fmt.Errorf("degoo: create directory placeholder failed and state could not be confirmed: %w", createErr)
		}
		return "", fmt.Errorf("degoo: directory placeholder may have been created but could not be resolved: %w", err)
	}
	if placeholder.ID == "" || placeholder.Name != leaf || placeholder.ParentID != pathID {
		return "", errors.New("degoo: created folder metadata does not match the requested child")
	}
	if placeholder.Category == 0 && createErr != nil {
		return "", errors.New("degoo: refusing to adopt a category-zero placeholder after an unconfirmed create")
	}
	if placeholder.Category != 0 && placeholder.Category != 6 {
		isDir, err := f.itemIsDirectory(ctx, placeholder)
		if err != nil {
			return "", err
		}
		if isDir {
			return placeholder.ID, nil
		}
		return "", fs.ErrorDirExists
	}
	if placeholder.Size != 0 {
		return "", fs.ErrorDirExists
	}

	markerName := directoryMarkerName(placeholder.ID)
	_, markerErr := f.api.Mkdir(ctx, placeholder.ID, markerName)
	if markerErr != nil && !api.Retryable(markerErr) {
		return "", fmt.Errorf("degoo: create directory marker: %w", markerErr)
	}
	directory, err := f.waitForCreatedDirectory(ctx, pathID, leaf, placeholder.ID, directoryPromotionTimeout)
	if err != nil {
		if markerErr != nil {
			return "", fmt.Errorf("degoo: create directory marker failed and state could not be confirmed: %w", markerErr)
		}
		return "", fmt.Errorf("degoo: directory marker may have been created but could not be verified: %w", err)
	}
	if err := f.retirePromotedPlaceholder(ctx, pathID, directory, []api.Item{placeholder}); err != nil {
		return "", fmt.Errorf("degoo: reconcile promoted directory: %w", err)
	}
	f.rememberPlaceholder(placeholder.ID)
	return directory.ID, nil
}

func (f *Fs) waitForItem(ctx context.Context, parentID, name string, timeout time.Duration) (api.Item, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	retryDelay := 250 * time.Millisecond
	for {
		if lookupCtx.Err() != nil {
			if ctx.Err() != nil {
				return api.Item{}, ctx.Err()
			}
			if lastErr != nil {
				return api.Item{}, lastErr
			}
			return api.Item{}, fs.ErrorDirNotFound
		}
		items, err := f.api.ListDirFresh(lookupCtx, parentID)
		delay := 250 * time.Millisecond
		if err == nil {
			lastErr = nil
			retryDelay = 250 * time.Millisecond
			matches := make([]api.Item, 0, 1)
			for _, item := range items {
				if item.Name == name {
					matches = append(matches, item)
				}
			}
			if len(matches) == 1 {
				return matches[0], nil
			}
			if len(matches) > 1 {
				return api.Item{}, fmt.Errorf("degoo returned %d items named %q", len(matches), name)
			}
		} else {
			if ctx.Err() != nil {
				return api.Item{}, ctx.Err()
			}
			if !api.Retryable(err) {
				return api.Item{}, err
			}
			lastErr = err
			delay = retryDelay
			if retryDelay < maxPacerSleep {
				retryDelay *= 2
				if retryDelay > maxPacerSleep {
					retryDelay = maxPacerSleep
				}
			}
		}

		if retryAfter, ok := pacer.IsRetryAfter(err); ok && retryAfter > delay {
			delay = retryAfter
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.Item{}, ctx.Err()
		case <-lookupCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return api.Item{}, ctx.Err()
			}
			if lastErr != nil {
				return api.Item{}, lastErr
			}
			return api.Item{}, fs.ErrorDirNotFound
		case <-timer.C:
		}
	}
}

func (f *Fs) waitForCreatedDirectory(ctx context.Context, parentID, name, placeholderID string, timeout time.Duration) (api.Item, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		items, err := f.listDir(ctx, parentID)
		if err == nil {
			matches := make([]api.Item, 0, 2)
			for _, item := range items {
				if item.Name == name && !isInternalName(item.Name) {
					matches = append(matches, item)
				}
			}
			if len(matches) != 0 {
				directory, isDir, _, resolveErr := f.resolveNameGroup(ctx, matches)
				if resolveErr != nil {
					lastErr = resolveErr
				} else if directory != nil && isDir {
					if directory.ID == placeholderID {
						found, markerErr := f.hasVerifiedDirectoryMarker(ctx, directory.ID, placeholderID)
						if markerErr != nil {
							lastErr = markerErr
						} else if found {
							return *directory, nil
						}
					} else if directory.Category == 2 {
						found, markerErr := f.hasVerifiedDirectoryMarker(ctx, directory.ID, placeholderID)
						if markerErr != nil {
							lastErr = markerErr
						} else if found {
							return *directory, nil
						}
					}
				}
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return api.Item{}, ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return api.Item{}, lastErr
			}
			return api.Item{}, errors.New("directory marker did not become visible")
		case <-ticker.C:
		}
	}
}

func (f *Fs) lookupChild(ctx context.Context, parentID, name string) (*api.Item, bool, []api.Item, error) {
	items, err := f.listDir(ctx, parentID)
	if err != nil {
		return nil, false, nil, err
	}
	return f.lookupChildInItems(ctx, name, items)
}

func (f *Fs) lookupChildFresh(ctx context.Context, parentID, name string) (*api.Item, bool, []api.Item, error) {
	items, err := f.listDirFresh(ctx, parentID)
	if err != nil {
		return nil, false, nil, err
	}
	return f.lookupChildInItems(ctx, name, items)
}

func (f *Fs) lookupChildInItems(ctx context.Context, name string, items []api.Item) (*api.Item, bool, []api.Item, error) {
	matches := make([]api.Item, 0, 1)
	var backups []api.Item
	for _, item := range items {
		if item.Name == name && !isInternalName(item.Name) {
			matches = append(matches, item)
			continue
		}
		if target, ok := backupTarget(item.Name); ok && target == name {
			backups = append(backups, item)
		}
	}
	if len(matches) == 0 {
		matches = backups
	}
	return f.resolveNameGroup(ctx, matches)
}

func (f *Fs) resolveNameGroup(ctx context.Context, matches []api.Item) (*api.Item, bool, []api.Item, error) {
	if len(matches) == 0 {
		return nil, false, nil, nil
	}
	if len(matches) == 1 {
		item := matches[0]
		if f.isProviderDirectoryCategory(item.Category) {
			return &item, true, nil, nil
		}
		if (item.Category == 0 || item.Category == 6) && item.Size == 0 {
			marker, err := f.hasVerifiedDirectoryMarker(ctx, item.ID, item.ID)
			if err != nil {
				return nil, false, nil, err
			}
			if marker {
				f.rememberPlaceholder(item.ID)
				return &item, true, nil, nil
			}
		}
		return &item, false, nil, nil
	}

	var realFolder *api.Item
	var placeholders []api.Item
	for i := range matches {
		item := matches[i]
		if item.Category == 2 {
			if realFolder != nil {
				return nil, false, nil, duplicateNameError(matches)
			}
			copy := item
			realFolder = &copy
		} else if (item.Category == 0 || item.Category == 6) && item.Size == 0 {
			placeholders = append(placeholders, item)
		}
	}
	if realFolder != nil && len(matches) == 2 && len(placeholders) == 1 {
		for _, placeholder := range placeholders {
			marker, err := f.hasDirectoryMarker(ctx, placeholder.ID)
			if err != nil {
				return nil, false, nil, err
			}
			if marker {
				f.rememberPlaceholder(placeholder.ID)
				return realFolder, true, []api.Item{placeholder}, nil
			}
			marker, err = f.hasNamedDirectoryMarker(ctx, realFolder.ID, directoryMarkerName(placeholder.ID))
			if err != nil {
				return nil, false, nil, err
			}
			if marker {
				f.rememberPlaceholder(placeholder.ID)
				return realFolder, true, []api.Item{placeholder}, nil
			}
		}
	}
	return nil, false, nil, duplicateNameError(matches)
}

func (f *Fs) retireConfiguredRootPlaceholder(ctx context.Context, root api.Item) error {
	if root.Category != 2 || root.ID == "" || root.ParentID == "" || root.ParentID == "0" {
		return nil
	}
	siblings, err := f.listDir(ctx, root.ParentID)
	if err != nil {
		return err
	}
	matches := make([]api.Item, 0, 2)
	for _, sibling := range siblings {
		if sibling.Name == root.Name {
			matches = append(matches, sibling)
		}
	}
	if len(matches) != 2 {
		return nil
	}
	for _, candidate := range matches {
		if candidate.ID == root.ID || candidate.Name != root.Name || candidate.ParentID != root.ParentID || candidate.Size != 0 || (candidate.Category != 0 && candidate.Category != 6) {
			continue
		}
		marker, err := f.hasVerifiedDirectoryMarker(ctx, root.ID, candidate.ID)
		if err != nil {
			return err
		}
		if !marker {
			marker, err = f.hasVerifiedDirectoryMarker(ctx, candidate.ID, candidate.ID)
			if err != nil {
				return err
			}
		}
		if !marker {
			continue
		}
		return f.retirePromotedPlaceholder(ctx, root.ParentID, root, []api.Item{candidate})
	}
	return nil
}

func (f *Fs) retirePromotedPlaceholder(ctx context.Context, parentID string, directory api.Item, placeholders []api.Item) error {
	if directory.Category != 2 || directory.ID == "" || len(placeholders) == 0 {
		return nil
	}
	if len(placeholders) != 1 {
		return errors.New("degoo: refusing to retire an ambiguous folder placeholder")
	}
	placeholder := placeholders[0]
	if placeholder.ID == directory.ID {
		return nil
	}
	if placeholder.ID == "" || placeholder.Name != directory.Name || placeholder.ParentID != parentID || directory.ParentID != parentID || placeholder.Size != 0 || (placeholder.Category != 0 && placeholder.Category != 6) {
		return errors.New("degoo: refusing to retire an unverified folder placeholder")
	}
	markerName := directoryMarkerName(placeholder.ID)
	deadline := time.NewTimer(directoryPromotionTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		children, err := f.listDir(ctx, placeholder.ID)
		if err != nil {
			return err
		}
		if len(children) > 1 || (len(children) == 1 && children[0].Name != markerName) {
			return errors.New("degoo: promoted folder placeholder contains data; refusing to rename it")
		}
		markerInDirectory, err := f.hasVerifiedDirectoryMarker(ctx, directory.ID, placeholder.ID)
		if err != nil {
			return err
		}
		if markerInDirectory && len(children) == 0 {
			return f.renameRetiredPlaceholder(ctx, parentID, directory, placeholder)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("degoo: folder promotion alias did not become safe to retire")
		case <-ticker.C:
		}
	}
}

func (f *Fs) renameRetiredPlaceholder(ctx context.Context, parentID string, directory, placeholder api.Item) error {
	retiredName := retiredDirectoryName(placeholder.ID)
	siblings, err := f.listDir(ctx, parentID)
	if err != nil {
		return err
	}
	for _, sibling := range siblings {
		if sibling.Name == retiredName {
			if sibling.ID == placeholder.ID && sibling.ParentID == parentID && sibling.Size == 0 && (sibling.Category == 0 || sibling.Category == 6) {
				return f.waitForRetiredPlaceholder(ctx, parentID, directory, placeholder, retiredName, 20*time.Second)
			}
			return errors.New("degoo: retired folder placeholder name is already in use")
		}
	}
	if err := f.renameConfirmed(ctx, placeholder.ID, parentID, retiredName); err != nil {
		return fmt.Errorf("degoo: retire verified folder placeholder: %w", err)
	}
	return f.waitForRetiredPlaceholder(ctx, parentID, directory, placeholder, retiredName, 20*time.Second)
}

func (f *Fs) waitForRetiredPlaceholder(ctx context.Context, parentID string, directory, placeholder api.Item, retiredName string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		items, err := f.listDir(ctx, parentID)
		if err == nil {
			retiredFound := false
			directoryFound := false
			oldNameCount := 0
			for _, item := range items {
				if item.Name == directory.Name {
					oldNameCount++
					if item.ID == directory.ID && item.Category == 2 {
						directoryFound = true
					}
				}
				if item.ID == placeholder.ID && item.Name == retiredName && item.ParentID == parentID && item.Size == 0 && (item.Category == 0 || item.Category == 6) {
					retiredFound = true
				}
			}
			if retiredFound && directoryFound && oldNameCount == 1 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if err != nil {
				return err
			}
			return errors.New("degoo: retired folder placeholder did not become visible")
		case <-ticker.C:
		}
	}
}

func duplicateNameError(items []api.Item) error {
	return fmt.Errorf("degoo: ambiguous duplicate name %q (%d matching items)", items[0].Name, len(items))
}

func (f *Fs) hasDirectoryMarker(ctx context.Context, directoryID string) (bool, error) {
	return f.hasNamedDirectoryMarker(ctx, directoryID, directoryMarkerName(directoryID))
}

func (f *Fs) hasNamedDirectoryMarker(ctx context.Context, directoryID, markerName string) (bool, error) {
	items, err := f.listDir(ctx, directoryID)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Name == markerName {
			return true, nil
		}
	}
	return false, nil
}

func (f *Fs) hasVerifiedDirectoryMarker(ctx context.Context, directoryID, placeholderID string) (bool, error) {
	items, err := f.listDir(ctx, directoryID)
	if err != nil {
		return false, err
	}
	markerName := directoryMarkerName(placeholderID)
	found := false
	for _, item := range items {
		if item.Name == markerName && item.ParentID == directoryID && item.Size == 0 && (item.Category == 0 || item.Category == 6) {
			if found {
				return false, errors.New("degoo: duplicate internal directory marker")
			}
			found = true
		}
	}
	return found, nil
}

func (f *Fs) itemIsDirectory(ctx context.Context, item api.Item) (bool, error) {
	if f.isProviderDirectoryCategory(item.Category) {
		return true, nil
	}
	if (item.Category == 0 || item.Category == 6) && item.Size == 0 {
		marker, err := f.hasVerifiedDirectoryMarker(ctx, item.ID, item.ID)
		return marker, err
	}
	return false, nil
}

func (f *Fs) isProviderDirectoryCategory(category int) bool {
	return category == 1 || category == 2
}

func (f *Fs) rememberPlaceholder(id string) {
	f.placeholderMu.Lock()
	f.placeholders[id] = struct{}{}
	f.placeholderMu.Unlock()
}

func (f *Fs) isPlaceholder(id string) bool {
	f.placeholderMu.RLock()
	_, ok := f.placeholders[id]
	f.placeholderMu.RUnlock()
	return ok
}

func directoryMarkerName(directoryID string) string {
	markerID := sha256.Sum256([]byte(directoryID))
	return directoryMarkerPrefix + hex.EncodeToString(markerID[:16])
}

func retiredDirectoryName(directoryID string) string {
	return retiredDirectoryPrefix + strings.TrimPrefix(directoryMarkerName(directoryID), directoryMarkerPrefix)
}

func newInternalName(prefix string) (string, error) {
	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return "", fmt.Errorf("degoo: generate internal name: %w", err)
	}
	return prefix + hex.EncodeToString(randomID[:]), nil
}

func newBackupName(leaf string) (string, error) {
	if err := validateLeaf(leaf); err != nil {
		return "", err
	}
	randomName, err := newInternalName("")
	if err != nil {
		return "", err
	}
	return backupPrefix + hex.EncodeToString([]byte(leaf)) + "-" + randomName, nil
}

func backupTarget(name string) (string, bool) {
	if !strings.HasPrefix(name, backupPrefix) {
		return "", false
	}
	payload := strings.TrimPrefix(name, backupPrefix)
	separator := strings.IndexByte(payload, '-')
	if separator <= 0 || len(payload)-separator-1 != 32 {
		return "", false
	}
	encoded := payload[:separator]
	if _, err := hex.DecodeString(payload[separator+1:]); err != nil {
		return "", false
	}
	leaf, err := hex.DecodeString(encoded)
	if err != nil || len(leaf) == 0 || validateLeaf(string(leaf)) != nil {
		return "", false
	}
	return string(leaf), true
}

func isInternalName(name string) bool {
	return strings.HasPrefix(name, directoryMarkerPrefix) || strings.HasPrefix(name, retiredDirectoryPrefix) || strings.HasPrefix(name, temporaryPrefix) || strings.HasPrefix(name, backupPrefix)
}

func normalizeRemote(remote string) (string, error) {
	if remote == "" {
		return "", nil
	}
	if path.IsAbs(remote) || strings.ContainsRune(remote, '\x00') {
		return "", errors.New("absolute paths and NUL bytes are not supported")
	}
	if path.Clean(remote) != remote {
		return "", errors.New("path must be clean and cannot contain empty, dot, or parent segments")
	}
	if err := validateRemote(remote); err != nil {
		return "", err
	}
	return remote, nil
}

func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return errors.New("endpoint must be an absolute HTTPS URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("endpoint must not contain credentials, query parameters, or a fragment")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		host := strings.ToLower(u.Hostname())
		ip := net.ParseIP(host)
		if host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return errors.New("endpoint must use HTTPS except for loopback test servers")
}

func validateRemote(remote string) error {
	if remote == "" {
		return nil
	}
	if path.IsAbs(remote) || strings.ContainsRune(remote, '\x00') {
		return errors.New("absolute paths and NUL bytes are not supported")
	}
	for _, part := range strings.Split(remote, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("path contains an unsafe segment")
		}
		if isInternalName(part) {
			return errors.New("path uses a reserved Degoo internal name")
		}
	}
	return nil
}

func validateLeaf(leaf string) error {
	if leaf == "" || leaf == "." || leaf == ".." || strings.ContainsAny(leaf, "/\x00") {
		return errors.New("invalid path component")
	}
	if isInternalName(leaf) {
		return errors.New("path component uses a reserved Degoo internal name")
	}
	return nil
}

// DirCacheFlush resets the directory ID cache.
func (f *Fs) DirCacheFlush() { f.dirCache.ResetRoot(); f.api.ClearMetadataCache() }

func (f *Fs) findDir(ctx context.Context, remote string, create bool) (string, error) {
	if err := validateRemote(remote); err != nil {
		return "", err
	}
	if remote != "" {
		if id, ok := f.dirCache.Get(remote); ok && f.isPlaceholder(id) {
			f.dirCache.FlushDir(remote)
		}
	}
	return f.dirCache.FindDir(ctx, remote, create)
}

// List lists files and directories directly below dir.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	if err := validateRemote(dir); err != nil {
		return nil, err
	}
	directoryID, err := f.findDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}
	items, err := f.listDir(ctx, directoryID)
	if err != nil {
		if isNotFoundError(err) {
			return nil, fs.ErrorDirNotFound
		}
		return nil, err
	}

	groups := make(map[string][]api.Item)
	backups := make(map[string][]api.Item)
	for _, item := range items {
		if target, ok := backupTarget(item.Name); ok {
			backups[target] = append(backups[target], item)
			continue
		}
		if isInternalName(item.Name) {
			continue
		}
		if err := validateLeaf(item.Name); err != nil {
			return nil, fmt.Errorf("degoo: provider returned an unsafe name: %w", err)
		}
		groups[item.Name] = append(groups[item.Name], item)
	}
	for name, backupItems := range backups {
		if _, exists := groups[name]; !exists {
			groups[name] = backupItems
		}
	}

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	slices.Sort(names)
	entries := make(fs.DirEntries, 0, len(names))
	for _, name := range names {
		item, isDir, ghosts, err := f.resolveNameGroup(ctx, groups[name])
		if err != nil {
			return nil, err
		}
		remote := path.Join(dir, name)
		if isDir {
			if err := f.retirePromotedPlaceholder(ctx, directoryID, *item, ghosts); err != nil {
				return nil, err
			}
			f.dirCache.Put(remote, item.ID)
			d := fs.NewDir(remote, itemModTime(*item)).SetID(item.ID).SetParentID(item.ParentID)
			entries = append(entries, d)
		} else {
			entries = append(entries, newObject(f, remote, *item))
		}
	}
	return entries, nil
}

// NewObject finds the file at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if err := validateRemote(remote); err != nil {
		return nil, err
	}
	if remote == "" {
		return nil, fs.ErrorIsDir
	}
	parentPath, leaf := dircache.SplitPath(remote)
	parentID, err := f.findDir(ctx, parentPath, false)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}
	item, isDir, _, err := f.lookupChild(ctx, parentID, leaf)
	if err != nil {
		if isNotFoundError(err) {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}
	if item == nil {
		return nil, fs.ErrorObjectNotFound
	}
	if isDir {
		return nil, fs.ErrorIsDir
	}
	return newObject(f, remote, *item), nil
}

func newObject(f *Fs, remote string, item api.Item) *Object {
	modTime := itemModTime(item)
	return &Object{
		fs:       f,
		remote:   remote,
		id:       item.ID,
		parentID: item.ParentID,
		name:     item.Name,
		size:     item.Size,
		modTime:  modTime,
		url:      item.URL,
		urlAt:    time.Now(),
	}
}

func itemModTime(item api.Item) time.Time {
	if !item.LastModificationTime.IsZero() {
		return item.LastModificationTime
	}
	return item.CreationTime
}

// Put stores an object at the path given by src.Remote.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	if err := validateRemote(remote); err != nil {
		return nil, err
	}
	if remote == "" {
		return nil, errors.New("degoo: cannot store an object at the vault root")
	}
	if existing, err := f.NewObject(ctx, remote); err == nil {
		if err := existing.(*Object).Update(ctx, in, src, options...); err != nil {
			return existing, err
		}
		return f.NewObject(ctx, remote)
	} else if !errors.Is(err, fs.ErrorObjectNotFound) {
		return nil, err
	}
	return f.uploadAndCommit(ctx, remote, nil, in, src.Size())
}

// PutStream stores an object whose size may not be known in advance.
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.Put(ctx, in, src, options...)
}

// Mkdir creates dir below the configured vault root.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	if err := validateRemote(dir); err != nil {
		return err
	}
	if dir == "" {
		_, err := f.findDir(ctx, "", true)
		return err
	}
	_, err := f.findDir(ctx, dir, true)
	return err
}

// Rmdir removes dir if it contains no user files or directories.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	if err := validateRemote(dir); err != nil {
		return err
	}
	if dir == "" {
		return errors.New("degoo: refusing to remove the configured vault root")
	}
	parentPath, leaf := dircache.SplitPath(dir)
	parentID, err := f.findDir(ctx, parentPath, false)
	if err != nil {
		return err
	}
	item, isDir, ghosts, err := f.lookupChild(ctx, parentID, leaf)
	if err != nil {
		return err
	}
	if item == nil {
		return fs.ErrorDirNotFound
	}
	if !isDir {
		return fs.ErrorIsFile
	}
	ids := []string{item.ID}
	for _, ghost := range ghosts {
		ids = append(ids, ghost.ID)
	}
	retired, err := f.findRetiredPlaceholders(ctx, parentID, *item)
	if err != nil {
		return err
	}
	for _, placeholder := range retired {
		ids = append(ids, placeholder.ID)
	}
	markerNames := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		markerNames[directoryMarkerName(id)] = struct{}{}
	}
	var markers []api.Item
	for _, id := range ids {
		children, err := f.listDir(ctx, id)
		if err != nil && !isNotFoundError(err) {
			return err
		}
		for _, child := range children {
			if _, isMarker := markerNames[child.Name]; !isMarker {
				return fs.ErrorDirectoryNotEmpty
			}
			markers = append(markers, child)
		}
	}
	for _, marker := range markers {
		if err := f.api.Delete(ctx, marker.ID, false); err != nil {
			return fmt.Errorf("degoo: remove directory marker: %w", err)
		}
	}
	if err := f.waitForIDsGoneInDirs(ctx, ids, itemIDs(markers), 20*time.Second); err != nil {
		return fmt.Errorf("degoo: wait for directory markers to be removed: %w", err)
	}
	for _, id := range ids {
		if err := f.api.Delete(ctx, id, false); err != nil && !isNotFoundError(err) {
			return fmt.Errorf("degoo: remove directory: %w", err)
		}
	}
	f.dirCache.FlushDir(dir)
	return nil
}

func (f *Fs) findRetiredPlaceholders(ctx context.Context, parentID string, directory api.Item) ([]api.Item, error) {
	children, err := f.listDir(ctx, directory.ID)
	if err != nil {
		return nil, err
	}
	markerSuffixes := make(map[string]struct{})
	for _, child := range children {
		if !strings.HasPrefix(child.Name, directoryMarkerPrefix) {
			continue
		}
		suffix := strings.TrimPrefix(child.Name, directoryMarkerPrefix)
		decoded, decodeErr := hex.DecodeString(suffix)
		if decodeErr == nil && len(decoded) == 16 && child.ParentID == directory.ID && child.Size == 0 && (child.Category == 0 || child.Category == 6) {
			markerSuffixes[suffix] = struct{}{}
		}
	}
	if len(markerSuffixes) == 0 {
		return nil, nil
	}
	siblings, err := f.listDir(ctx, parentID)
	if err != nil {
		return nil, err
	}
	var retired []api.Item
	for _, sibling := range siblings {
		if !strings.HasPrefix(sibling.Name, retiredDirectoryPrefix) {
			continue
		}
		suffix := strings.TrimPrefix(sibling.Name, retiredDirectoryPrefix)
		if _, ok := markerSuffixes[suffix]; !ok || retiredDirectoryName(sibling.ID) != sibling.Name || sibling.ParentID != parentID || sibling.Size != 0 || (sibling.Category != 0 && sibling.Category != 6) {
			continue
		}
		placeholderChildren, err := f.listDir(ctx, sibling.ID)
		if err != nil {
			return nil, err
		}
		if len(placeholderChildren) != 0 {
			return nil, errors.New("degoo: retired folder placeholder contains data; refusing to remove directory")
		}
		retired = append(retired, sibling)
	}
	return retired, nil
}

// Move moves src to remote using Degoo's move and rename mutations.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok || !f.sameVault(srcObj.fs) {
		return nil, fs.ErrorCantMove
	}
	if err := validateRemote(srcObj.remote); err != nil {
		return nil, err
	}
	if srcObj.remote == "" || srcObj.id == f.opt.RootFolderID {
		return nil, errors.New("degoo: refusing to move the configured vault root")
	}
	if err := validateRemote(remote); err != nil {
		return nil, err
	}
	if remote == "" {
		return nil, errors.New("degoo: cannot move an object to the vault root")
	}
	if srcObj.remote == remote {
		return f.NewObject(ctx, remote)
	}
	parentPath, dstLeaf := dircache.SplitPath(remote)
	dstParentID, err := f.findDir(ctx, parentPath, true)
	if err != nil {
		return nil, err
	}
	if existing, err := f.NewObject(ctx, remote); err == nil {
		return nil, fmt.Errorf("degoo: move destination exists: %q", existing.Remote())
	} else if errors.Is(err, fs.ErrorIsDir) {
		return nil, fs.ErrorDirExists
	} else if !errors.Is(err, fs.ErrorObjectNotFound) {
		return nil, err
	}
	if srcObj.parentID != dstParentID {
		if err := f.moveConfirmed(ctx, srcObj.id, dstParentID); err != nil {
			return nil, fmt.Errorf("degoo: move object: %w", err)
		}
	}
	if srcObj.name != dstLeaf {
		if err := f.renameConfirmed(ctx, srcObj.id, dstParentID, dstLeaf); err != nil {
			return nil, fmt.Errorf("degoo: object moved but rename failed: %w", err)
		}
	}
	return f.NewObject(ctx, remote)
}

// DirMove moves a directory within the same configured vault.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok || !f.sameVault(srcFs) {
		return fs.ErrorCantDirMove
	}
	if err := validateRemote(srcRemote); err != nil {
		return err
	}
	if err := validateRemote(dstRemote); err != nil {
		return err
	}
	if srcRemote == "" || dstRemote == "" {
		return errors.New("degoo: cannot move the configured vault root")
	}
	if dstRemote == srcRemote || strings.HasPrefix(dstRemote, srcRemote+"/") {
		return errors.New("degoo: cannot move a directory into itself")
	}
	srcID, srcParentID, srcLeaf, dstParentID, dstLeaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err
	}
	if existing, _, _, err := f.lookupChild(ctx, dstParentID, dstLeaf); err != nil {
		return err
	} else if existing != nil {
		return fs.ErrorDirExists
	}
	if srcParentID != dstParentID {
		if err := f.moveConfirmed(ctx, srcID, dstParentID); err != nil {
			return fmt.Errorf("degoo: move directory: %w", err)
		}
	}
	if srcLeaf != dstLeaf {
		if err := f.renameConfirmed(ctx, srcID, dstParentID, dstLeaf); err != nil {
			return fmt.Errorf("degoo: directory moved but rename failed: %w", err)
		}
	}
	srcFs.dirCache.FlushDir(srcRemote)
	f.dirCache.FlushDir(dstRemote)
	return nil
}

// Open opens the object, validating any requested byte range.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	if err := validateRemote(o.remote); err != nil {
		return nil, err
	}
	if o.remote == "" || o.id == o.fs.opt.RootFolderID {
		return nil, fs.ErrorIsDir
	}
	openOptions := append([]fs.OpenOption(nil), options...)
	fs.FixRangeOption(openOptions, o.size)
	rangeHeader := ""
	for _, option := range openOptions {
		key, value := option.Header()
		if strings.EqualFold(key, "Range") {
			if rangeHeader != "" {
				return nil, errors.New("degoo: multiple range requests are not supported")
			}
			rangeHeader = value
		}
	}
	downloadURL := o.url
	refreshURL := downloadURL == "" || time.Since(o.urlAt) >= downloadURLMaxAge
	if refreshURL {
		item, err := o.fs.getItem(ctx, o.id)
		if err != nil {
			return nil, fmt.Errorf("degoo: refresh download metadata: %w", err)
		}
		downloadURL = item.URL
	}
	if downloadURL == "" {
		return nil, errors.New("degoo: item has no download URL")
	}
	resp, err := o.fs.api.Download(ctx, downloadURL, rangeHeader)
	if err != nil && !refreshURL && (api.HTTPStatus(err) == http.StatusUnauthorized || api.HTTPStatus(err) == http.StatusForbidden) {
		item, refreshErr := o.fs.getItem(ctx, o.id)
		if refreshErr != nil {
			return nil, fmt.Errorf("degoo: refresh expired download URL: %w", refreshErr)
		}
		if item.URL == "" {
			return nil, errors.New("degoo: item has no download URL")
		}
		resp, err = o.fs.api.Download(ctx, item.URL, rangeHeader)
	}
	if err != nil {
		return nil, err
	}
	expected, known, err := expectedReadLength(o.size, openOptions)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	if known && resp.ContentLength >= 0 && resp.ContentLength != expected {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("degoo: download length mismatch: got %d, want %d", resp.ContentLength, expected)
	}
	return &verifiedReadCloser{ReadCloser: resp.Body, expected: expected, known: known}, nil
}

func expectedReadLength(size int64, options []fs.OpenOption) (int64, bool, error) {
	if size < 0 {
		return 0, false, nil
	}
	for _, option := range options {
		if rangeOption, ok := option.(*fs.RangeOption); ok {
			offset, limit := rangeOption.Decode(size)
			if offset > size {
				return 0, false, fmt.Errorf("degoo: range starts past end of object")
			}
			if limit < 0 || limit > size-offset {
				limit = size - offset
			}
			return limit, true, nil
		}
	}
	return size, true, nil
}

type verifiedReadCloser struct {
	io.ReadCloser
	expected int64
	read     int64
	known    bool
}

func (r *verifiedReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.read += int64(n)
	if err == io.EOF && r.known && r.read != r.expected {
		return n, fmt.Errorf("degoo: truncated response: read %d of %d bytes: %w", r.read, r.expected, io.ErrUnexpectedEOF)
	}
	if r.known && r.read > r.expected {
		return n, fmt.Errorf("degoo: response exceeded requested length %d", r.expected)
	}
	return n, err
}

// Remove moves the object to Degoo's recycle bin.
func (o *Object) Remove(ctx context.Context) error {
	if err := validateRemote(o.remote); err != nil {
		return err
	}
	if o.remote == "" || o.id == o.fs.opt.RootFolderID {
		return errors.New("degoo: refusing to remove the configured vault root")
	}
	_, leaf := dircache.SplitPath(o.remote)
	if o.parentID == "" || o.parentID == "0" {
		return errors.New("degoo: refusing to delete an object without a verified parent")
	}
	items, err := o.fs.listDir(ctx, o.parentID)
	if err != nil {
		return fmt.Errorf("degoo: inspect object before deletion: %w", err)
	}
	var active []api.Item
	var backups []api.Item
	for _, item := range items {
		if item.Name == leaf {
			if item.ParentID != o.parentID {
				return errors.New("degoo: listed object has an unexpected parent")
			}
			active = append(active, item)
			continue
		}
		if target, ok := backupTarget(item.Name); ok && target == leaf {
			if item.ParentID != o.parentID {
				return errors.New("degoo: listed backup has an unexpected parent")
			}
			backups = append(backups, item)
		}
	}
	if len(active) > 1 {
		return duplicateNameError(active)
	}
	var target *api.Item
	deleteBackups := false
	if len(active) == 1 && active[0].ID == o.id {
		target = &active[0]
		deleteBackups = true
	} else {
		var matchingBackup *api.Item
		for i := range backups {
			if backups[i].ID == o.id {
				matchingBackup = &backups[i]
				break
			}
		}
		if matchingBackup == nil {
			return fs.ErrorObjectNotFound
		}
		if len(backups) != 1 {
			return duplicateNameError(backups)
		}
		// A stale handle may identify an older version while a replacement is active.
		target = matchingBackup
	}
	if deleteBackups {
		for _, backup := range backups {
			if err := o.fs.api.Delete(ctx, backup.ID, false); err != nil {
				return fmt.Errorf("degoo: remove hidden backup before deleting object: %w", err)
			}
		}
		if len(backups) != 0 {
			if err := o.fs.waitForNoBackups(ctx, o.parentID, leaf, 20*time.Second); err != nil {
				return fmt.Errorf("degoo: hidden backup may still be recoverable; active object retained: %w", err)
			}
		}
	}
	if err := o.fs.api.Delete(ctx, target.ID, false); err != nil {
		return fmt.Errorf("degoo: delete object: %w", err)
	}
	if err := o.fs.waitForIDsGone(ctx, o.parentID, []string{target.ID}, 20*time.Second); err != nil {
		return fmt.Errorf("degoo: wait for object deletion: %w", err)
	}
	return nil
}

func itemIDs(items []api.Item) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func (f *Fs) waitForNoBackups(ctx context.Context, parentID, leaf string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		items, err := f.listDir(ctx, parentID)
		if err != nil {
			lastErr = err
		} else {
			found := false
			for _, item := range items {
				if target, ok := backupTarget(item.Name); ok && target == leaf {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return lastErr
			}
			return errors.New("hidden backups remain visible")
		case <-ticker.C:
		}
	}
}

func (f *Fs) waitForIDsGone(ctx context.Context, parentID string, ids []string, timeout time.Duration) error {
	return f.waitForIDsGoneInDirs(ctx, []string{parentID}, ids, timeout)
}

func (f *Fs) waitForIDsGoneInDirs(ctx context.Context, parentIDs, ids []string, timeout time.Duration) error {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	if len(wanted) == 0 {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		present := make(map[string]struct{})
		var listingErr error
		for _, parentID := range parentIDs {
			items, err := f.listDirFresh(ctx, parentID)
			if err != nil {
				if isNotFoundError(err) {
					continue
				}
				listingErr = err
				break
			}
			for _, item := range items {
				if _, ok := wanted[item.ID]; ok {
					present[item.ID] = struct{}{}
				}
			}
		}
		lastErr = listingErr
		if len(present) == 0 && listingErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return lastErr
			}
			return errors.New("deleted items remain visible")
		case <-ticker.C:
		}
	}
}

// Update replaces the object only after its new encrypted bytes are uploaded and verified.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if err := validateRemote(o.remote); err != nil {
		return err
	}
	if o.remote == "" || o.id == o.fs.opt.RootFolderID {
		return errors.New("degoo: refusing to replace the configured vault root")
	}
	if src.Remote() != o.remote {
		return errors.New("degoo: object update path does not match source path")
	}
	_, err := o.fs.uploadAndCommit(ctx, o.remote, o, in, src.Size())
	return err
}

func (f *Fs) uploadAndCommit(ctx context.Context, remote string, old *Object, in io.Reader, expectedSize int64) (*Object, error) {
	if err := validateRemote(remote); err != nil {
		return nil, err
	}
	if remote == "" {
		return nil, errors.New("degoo: cannot store an object at the vault root")
	}
	if in == nil {
		return nil, errors.New("degoo: nil upload reader")
	}
	if expectedSize < -1 {
		return nil, fmt.Errorf("degoo: invalid upload size %d", expectedSize)
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()

	parentPath, leaf := dircache.SplitPath(remote)
	parentID, err := f.findDir(ctx, parentPath, true)
	if err != nil {
		return nil, err
	}
	current, isDir, _, err := f.lookupChild(ctx, parentID, leaf)
	if err != nil {
		return nil, err
	}
	if old == nil {
		if current != nil {
			if isDir {
				return nil, fs.ErrorIsDir
			}
			return nil, fmt.Errorf("degoo: object already exists: %q", remote)
		}
	} else {
		if current == nil || current.ID != old.id || isDir {
			return nil, errors.New("degoo: target changed before update; refusing to replace it")
		}
	}

	var stagedFile *os.File
	var stagedSize int64
	var localDigest [sha256.Size]byte
	var uploadReader io.Reader
	var providerChecksum string
	if expectedSize >= 0 {
		prefix, err := readChecksumPrefix(in, expectedSize)
		if err != nil {
			return nil, err
		}
		stagedSize = expectedSize
		providerChecksum, err = api.Checksum(bytes.NewReader(prefix))
		if err != nil {
			return nil, err
		}
		uploadReader = io.MultiReader(bytes.NewReader(prefix), in)
	} else {
		var err error
		stagedFile, stagedSize, localDigest, err = spoolUpload(in, expectedSize)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = stagedFile.Close()
			_ = os.Remove(stagedFile.Name())
		}()
		providerChecksum, err = api.Checksum(stagedFile)
		if err != nil {
			return nil, err
		}
		if _, err := stagedFile.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		uploadReader = stagedFile
	}
	tempName, err := newInternalName(temporaryPrefix)
	if err != nil {
		return nil, err
	}
	tempName += ".bin"
	writeAuth, authErr := f.api.GetWriteAuth(ctx, parentID, tempName, stagedSize, providerChecksum)
	if authErr != nil && !errors.Is(authErr, api.ErrAlreadyExists) {
		return nil, fmt.Errorf("degoo: get upload authorization: %w", authErr)
	}
	if authErr == nil {
		if stagedFile != nil {
			if _, err := stagedFile.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
		}
		digestReader := newDigestReader(uploadReader)
		if err := f.api.Upload(ctx, writeAuth, tempName, providerChecksum, stagedSize, "application/octet-stream", digestReader); err != nil {
			return nil, fmt.Errorf("degoo: upload encrypted bytes (unregistered storage may remain): %w", err)
		}
		if digestReader.n != stagedSize {
			return nil, fmt.Errorf("degoo: upload read %d bytes, want %d; pending storage may remain", digestReader.n, stagedSize)
		}
		if stagedFile == nil {
			localDigest = digestReader.sum()
		}
	} else if stagedFile == nil {
		digestReader := newDigestReader(uploadReader)
		if _, err := io.Copy(io.Discard, digestReader); err != nil {
			return nil, fmt.Errorf("degoo: read content for existing Degoo blob: %w", err)
		}
		if digestReader.n != stagedSize {
			return nil, fmt.Errorf("degoo: upload size mismatch: read %d bytes, source reported %d", digestReader.n, stagedSize)
		}
		localDigest = digestReader.sum()
	}
	created := time.Now()
	_, registerErr := f.api.Register(ctx, parentID, tempName, providerChecksum, stagedSize, created)
	if registerErr != nil {
		fs.Logf(f, "degoo: reconciling encrypted upload after unacknowledged registration (parent ID %q, %d bytes): %v", parentID, stagedSize, registerErr)
	}
	// An unacknowledged mutation can become visible after a delayed listing.
	// Reconcile the exact temporary name before returning this attempt.
	lookupTimeout := uploadRegistrationTimeout
	if errors.Is(registerErr, api.ErrNegativeAcknowledgement) {
		lookupTimeout = negativeRegistrationTimeout
	}
	item, err := f.waitForItem(ctx, parentID, tempName, lookupTimeout)
	if err != nil {
		if registerErr != nil {
			return nil, fmt.Errorf("degoo: register uploaded data; result is ambiguous and pending storage may remain: %w; metadata confirmation failed: %v", registerErr, err)
		}
		return nil, fmt.Errorf("degoo: uploaded bytes are not yet registered; pending storage retained: %w", err)
	}
	return f.finishUpload(ctx, remote, leaf, parentID, item, old, stagedSize, localDigest)
}

func readChecksumPrefix(in io.Reader, size int64) ([]byte, error) {
	prefixSize := int64(1024 * 1024)
	if size < prefixSize {
		prefixSize = size
	}
	prefix := make([]byte, prefixSize)
	read, err := io.ReadFull(in, prefix)
	if err != nil {
		return nil, fmt.Errorf("degoo: read checksum prefix: %w", err)
	}
	if int64(read) != prefixSize {
		return nil, fmt.Errorf("degoo: read %d checksum prefix bytes, want %d", read, prefixSize)
	}
	return prefix, nil
}

type digestReader struct {
	reader io.Reader
	hasher stdhash.Hash
	n      int64
}

func newDigestReader(reader io.Reader) *digestReader {
	return &digestReader{reader: reader, hasher: sha256.New()}
}

func (reader *digestReader) Read(p []byte) (int, error) {
	n, err := reader.reader.Read(p)
	if n > 0 {
		_, _ = reader.hasher.Write(p[:n])
		reader.n += int64(n)
	}
	return n, err
}

func (reader *digestReader) sum() [sha256.Size]byte {
	var digest [sha256.Size]byte
	copy(digest[:], reader.hasher.Sum(nil))
	return digest
}

func spoolUpload(in io.Reader, expectedSize int64) (*os.File, int64, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	file, err := os.CreateTemp("", "rclone-degoo-upload-")
	if err != nil {
		return nil, 0, digest, fmt.Errorf("degoo: create secure upload spool: %w", err)
	}
	cleanup := func(err error) (*os.File, int64, [sha256.Size]byte, error) {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, 0, digest, err
	}
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hasher), in)
	if err != nil {
		return cleanup(fmt.Errorf("degoo: spool upload: %w", err))
	}
	if expectedSize >= 0 && size != expectedSize {
		return cleanup(fmt.Errorf("degoo: upload size mismatch: read %d bytes, source reported %d", size, expectedSize))
	}
	copy(digest[:], hasher.Sum(nil))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return cleanup(fmt.Errorf("degoo: rewind upload spool: %w", err))
	}
	return file, size, digest, nil
}

func (f *Fs) finishUpload(ctx context.Context, remote, leaf, parentID string, staged api.Item, old *Object, size int64, digest [sha256.Size]byte) (*Object, error) {
	if staged.ID == "" {
		return nil, errors.New("degoo: registered upload has no item ID")
	}
	if staged.Size != size {
		return nil, fmt.Errorf("degoo: registered upload size mismatch: got %d, want %d", staged.Size, size)
	}
	if staged.ParentID != parentID || staged.Name == "" || !strings.HasPrefix(staged.Name, temporaryPrefix) {
		return nil, errors.New("degoo: registered upload metadata does not match the pending object")
	}
	verifiedItem, err := f.verifyUploaded(ctx, staged.ID, size, digest, !f.opt.SkipUploadVerification)
	if err != nil {
		return nil, fmt.Errorf("degoo: uploaded object verification failed; pending data retained: %w", err)
	}

	var backupName string
	if old != nil {
		var err error
		backupName, err = newBackupName(leaf)
		if err != nil {
			return nil, err
		}
		if err := f.renameConfirmed(ctx, old.id, old.parentID, backupName); err != nil {
			return nil, fmt.Errorf("degoo: preserve prior object before publishing replacement: %w", err)
		}
	}
	if err := f.renameConfirmed(ctx, staged.ID, parentID, leaf); err != nil {
		if old != nil {
			if errors.Is(err, errMutationAmbiguous) {
				return nil, fmt.Errorf("degoo: replacement publication is ambiguous; prior object remains recoverable under its internal backup name: %w", err)
			}
			if rollbackErr := f.renameConfirmed(ctx, old.id, old.parentID, leaf); rollbackErr != nil {
				return nil, fmt.Errorf("degoo: publish replacement failed: %v; prior object remains recoverable under its internal backup name: %w", err, rollbackErr)
			}
		}
		return nil, fmt.Errorf("degoo: publish verified upload: %w", err)
	}

	published, err := f.waitForPublishedObject(ctx, remote, leaf, parentID, staged, verifiedItem.ParentID, size, 20*time.Second)
	if err != nil {
		return nil, fmt.Errorf("degoo: verified upload was published but cannot be resolved; prior backup retained: %w", err)
	}
	if old != nil {
		if err := f.api.Delete(ctx, old.id, false); err != nil {
			fs.Errorf(f, "could not remove hidden backup after replacement; new object is committed: %v", err)
		}
	}
	fs.Logf(f, "degoo: remote object published (ID %q, parent ID %q, %d bytes, readback verification %t); logical file upload may still be pending", published.id, parentID, size, !f.opt.SkipUploadVerification)
	return published, nil
}

func (f *Fs) waitForPublishedObject(ctx context.Context, remote, leaf, parentID string, staged api.Item, preRenameParentID string, size int64, timeout time.Duration) (*Object, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		item, err := f.getItem(ctx, staged.ID)
		if err != nil {
			lastErr = err
		} else if item.ID != staged.ID || item.Name != leaf || item.Size != size {
			lastErr = fmt.Errorf("published metadata mismatch (id_match=%t name_match=%t parent_match=%t parent_empty=%t parent_is_zero=%t got_size=%d want_size=%d category=%d)",
				item.ID == staged.ID, item.Name == leaf, item.ParentID == parentID, item.ParentID == "", item.ParentID == "0", item.Size, size, item.Category)
		} else {
			overlayDiagnostic := ""
			if item.ParentID != parentID {
				overlayDiagnostic = fmt.Sprintf("overlay parent metadata differs (pre_parent_expected=%t pre_parent_staged=%t pre_parent_vault=%t post_parent_expected=%t post_parent_staged=%t post_parent_vault=%t post_parent_same_as_pre=%t post_parent_empty=%t post_parent_is_zero=%t category=%d)",
					preRenameParentID == parentID, preRenameParentID == staged.ParentID, preRenameParentID == f.opt.RootFolderID,
					item.ParentID == parentID, item.ParentID == staged.ParentID, item.ParentID == f.opt.RootFolderID,
					item.ParentID == preRenameParentID, item.ParentID == "", item.ParentID == "0", item.Category)
			}
			if item.ParentID == parentID && item.Size > 0 && !f.isProviderDirectoryCategory(item.Category) {
				return newObject(f, remote, item), nil
			}
			visible, isDir, _, listErr := f.lookupChildFresh(ctx, parentID, leaf)
			if listErr != nil {
				lastErr = listErr
				if overlayDiagnostic != "" {
					lastErr = fmt.Errorf("%s; canonical listing: %w", overlayDiagnostic, listErr)
				}
			} else if visible != nil && !isDir && visible.ID == staged.ID && visible.ParentID == parentID && visible.Size == size {
				return newObject(f, remote, *visible), nil
			} else {
				lastErr = errors.New("published listing does not match the verified upload")
				if overlayDiagnostic != "" {
					lastErr = fmt.Errorf("%s; canonical listing does not match the verified upload", overlayDiagnostic)
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, errors.New("published object did not become visible")
		case <-ticker.C:
		}
	}
}

func (f *Fs) verifyUploaded(ctx context.Context, id string, size int64, want [sha256.Size]byte, verifyPayload bool) (api.Item, error) {
	item, err := f.getItem(ctx, id)
	if err != nil {
		return api.Item{}, err
	}
	if item.ID != id || item.Size != size {
		return api.Item{}, errors.New("degoo metadata ID or size is invalid")
	}
	if !verifyPayload {
		return item, nil
	}
	if item.URL == "" {
		return api.Item{}, errors.New("degoo metadata download URL is invalid")
	}
	resp, err := f.api.Download(ctx, item.URL, "")
	if err != nil {
		return api.Item{}, err
	}
	hasher := sha256.New()
	readSize, readErr := io.Copy(hasher, resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return api.Item{}, readErr
	}
	if closeErr != nil {
		return api.Item{}, closeErr
	}
	if readSize != size {
		return api.Item{}, fmt.Errorf("downloaded %d bytes, want %d", readSize, size)
	}
	if !strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), hex.EncodeToString(want[:])) {
		return api.Item{}, errors.New("downloaded bytes do not match the spooled upload")
	}
	return item, nil
}

func (f *Fs) renameConfirmed(ctx context.Context, id, parentID, name string) error {
	requestErr := f.api.Rename(ctx, id, name)
	if requestErr == nil {
		return nil
	}
	item, err := f.getItem(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: rename request could not be reconciled: %v", errMutationAmbiguous, requestErr)
	}
	if item.ID == id && item.ParentID == parentID && item.Name == name {
		return nil
	}
	return fmt.Errorf("rename request failed and item state could not confirm the requested name: %w", requestErr)
}

func (f *Fs) moveConfirmed(ctx context.Context, id, parentID string) error {
	requestErr := f.api.Move(ctx, id, parentID)
	if requestErr == nil {
		return nil
	}
	item, err := f.getItem(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: move request could not be reconciled: %v", errMutationAmbiguous, requestErr)
	}
	if item.ID == id && item.ParentID == parentID {
		return nil
	}
	return fmt.Errorf("move request failed and item state shows parent %q: %w", item.ParentID, requestErr)
}

// SetModTime reports that Degoo does not expose a modification-time mutation.
func (o *Object) SetModTime(context.Context, time.Time) error { return fs.ErrorCantSetModTime }

// Fs returns the filesystem containing the object.
func (o *Object) Fs() fs.Info { return o.fs }

// String returns a description of the object.
func (o *Object) String() string { return o.remote }

// Remote returns the path of the object.
func (o *Object) Remote() string { return o.remote }

// ModTime returns the provider modification time.
func (o *Object) ModTime(context.Context) time.Time { return o.modTime }

// Size returns the object size in bytes.
func (o *Object) Size() int64 { return o.size }

// Hash returns an error because Degoo's seeded prefix checksum is not an rclone hash.
func (o *Object) Hash(context.Context, hash.Type) (string, error) { return "", hash.ErrUnsupported }

// Storable reports whether this object can be stored.
func (o *Object) Storable() bool { return true }

// ID returns the provider ID of the object.
func (o *Object) ID() string { return o.id }

// ParentID returns the provider ID of the parent directory.
func (o *Object) ParentID() string { return o.parentID }

func (f *Fs) listDir(ctx context.Context, parentID string) ([]api.Item, error) {
	var items []api.Item
	err := f.metadataCall(ctx, func(callCtx context.Context) error {
		var err error
		items, err = f.api.ListDir(callCtx, parentID)
		return err
	})
	return items, err
}

func (f *Fs) listDirFresh(ctx context.Context, parentID string) ([]api.Item, error) {
	var items []api.Item
	err := f.metadataCall(ctx, func(callCtx context.Context) error {
		var err error
		items, err = f.api.ListDirFresh(callCtx, parentID)
		return err
	})
	return items, err
}

func (f *Fs) getItem(ctx context.Context, id string) (api.Item, error) {
	var item api.Item
	err := f.metadataCall(ctx, func(callCtx context.Context) error {
		var err error
		item, err = f.api.GetItem(callCtx, id)
		return err
	})
	return item, err
}

func (f *Fs) metadataCall(ctx context.Context, call func(context.Context) error) error {
	return metadataCallWithTimeout(ctx, metadataLookupTimeout, call)
}

func metadataCallWithTimeout(ctx context.Context, timeout time.Duration, call func(context.Context) error) error {
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	delay := metadataRetryDelay
	for attempt := 0; attempt < metadataMaxAttempts; attempt++ {
		if err := lookupCtx.Err(); err != nil {
			return err
		}
		err := call(lookupCtx)
		if err == nil || !api.Retryable(err) || attempt+1 == metadataMaxAttempts {
			return err
		}
		if _, hasRetryAfter := pacer.IsRetryAfter(err); hasRetryAfter {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-lookupCtx.Done():
			timer.Stop()
			return lookupCtx.Err()
		case <-timer.C:
		}
		delay *= 2
	}
	return nil
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if api.HTTPStatus(err) == http.StatusNotFound {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "notfound")
}

var _ fs.Fs = (*Fs)(nil)
var _ dircache.DirCacher = (*Fs)(nil)
var _ fs.Object = (*Object)(nil)
