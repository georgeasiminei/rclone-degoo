//go:build linux

package degoo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/lib/version"
	"github.com/spf13/cobra"
)

const (
	vaultCheckTimeout       = 45 * time.Second
	vaultCheckPageSize      = 128
	vaultCheckMaxPages      = 32
	vaultCheckMaxItems      = 1024
	vaultCheckMaxDirs       = 32
	vaultCheckMaxDepth      = 8
	vaultCheckPlainBlock    = 64 * 1024
	vaultCheckMarkerPrefix  = ".rclone-degoo-dir-marker-"
	vaultCheckRetiredPrefix = ".rclone-degoo-retired-dir-"
)

const (
	vaultCheckEmpty    = "empty"
	vaultCheckVerified = "verified"
)

type setupAPIFunc func(context.Context, configmap.Getter) (*api.Client, error)

type vaultCheckDirectory struct {
	id    string
	depth int
}

type vaultCheckBudget struct {
	pages int
	items int
}

// newCheckVaultCommand builds a read-only folder verifier using the supplied
// API factory.
func newCheckVaultCommand(makeAPI setupAPIFunc) *cobra.Command {
	var vaultConfig, folderID string
	command := &cobra.Command{
		Use:   "check-vault auth.json",
		Short: "Check whether a Degoo folder is empty or encrypted by a vault password.",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := safeLogging(c.Context()); err != nil {
				return err
			}
			if vaultConfig == "" {
				return errors.New("--vault-config is required")
			}
			if strings.TrimSpace(folderID) != folderID || folderID == "" || folderID == "0" || len(folderID) > 4096 {
				return errors.New("--folder-id must identify a dedicated Degoo folder")
			}
			cfg, err := readConfig(vaultConfig)
			if err != nil {
				return fmt.Errorf("read vault configuration: %w", err)
			}

			ctx, cancel := context.WithTimeout(c.Context(), vaultCheckTimeout)
			defer cancel()
			m := transportDefaults()
			m["auth_file"] = args[0]
			credentialsFile, err := credentialsFileForAuth(args[0], vaultConfig)
			if err != nil {
				return err
			}
			if credentialsFile != "" {
				m["credentials_file"] = credentialsFile
			}
			client, err := makeAPI(ctx, m)
			if err != nil {
				return err
			}
			status, err := checkVaultFolder(ctx, client, cfg, folderID)
			if err != nil {
				return err
			}
			return json.NewEncoder(c.OutOrStdout()).Encode(struct {
				Status string `json:"status"`
			}{Status: status})
		},
	}
	command.Flags().StringVar(&vaultConfig, "vault-config", "", "Private vault configuration containing the candidate encryption password")
	command.Flags().StringVar(&folderID, "folder-id", "", "Dedicated Degoo folder ID to check")
	return command
}

func vaultCheckCipher(cfg *VaultConfig) (*crypt.Cipher, error) {
	password, err := obscure.Obscure(cfg.Password)
	if err != nil {
		return nil, errors.New("could not prepare vault password verifier")
	}
	salt, err := obscure.Obscure(cfg.PasswordSalt)
	if err != nil {
		return nil, errors.New("could not prepare vault password verifier")
	}
	cipher, err := crypt.NewCipher(configmap.Simple{
		"password":                  password,
		"password2":                 salt,
		"filename_encryption":       "standard",
		"filename_encoding":         "base32",
		"directory_name_encryption": "true",
		"no_data_encryption":        "false",
		"suffix":                    ".bin",
	})
	if err != nil {
		return nil, errors.New("vault cipher settings are invalid")
	}
	return cipher, nil
}

func checkVaultFolder(ctx context.Context, client *api.Client, cfg *VaultConfig, folderID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cipher, err := vaultCheckCipher(cfg)
	if err != nil {
		return "", err
	}
	root, err := client.GetItem(ctx, folderID)
	if err != nil {
		return "", fmt.Errorf("inspect selected Degoo folder: %w", err)
	}
	if root.ID != folderID || root.Name == "" || root.ParentID == "" || root.ParentID == "0" || root.ParentID == folderID || root.Size < 0 {
		return "", errors.New("selected Degoo item is not a dedicated folder")
	}
	rootIsPlaceholder := (root.Category == 0 || root.Category == 6) && root.Size == 0
	if root.Category != 2 && !rootIsPlaceholder {
		return "", errors.New("selected Degoo item is not a dedicated folder")
	}
	budget := &vaultCheckBudget{}
	if rootIsPlaceholder {
		verified, err := hasVaultCheckSelfMarker(ctx, client, folderID, budget)
		if err != nil {
			return "", fmt.Errorf("verify selected folder placeholder: %w", err)
		}
		if !verified {
			return "", errors.New("selected Degoo placeholder has no verified self marker")
		}
	}

	queue := []vaultCheckDirectory{{id: folderID}}
	seenDirectories := map[string]struct{}{folderID: {}}
	seenItems := map[string]struct{}{folderID: {}}
	rootHasContent := false
	for queueIndex := 0; queueIndex < len(queue); queueIndex++ {
		directory := queue[queueIndex]
		pageToken := ""
		seenCursors := make(map[string]struct{})
		for {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			items, nextToken, err := listVaultCheckPage(ctx, client, directory.id, pageToken, budget)
			if err != nil {
				return "", fmt.Errorf("list selected Degoo folder: %w", err)
			}

			// Validate every listed entry before trusting any one file's content. A
			// valid encrypted sample must not hide plaintext or malformed siblings.
			var candidates []api.Item
			var childDirectories []vaultCheckDirectory
			var possiblePlaceholders []api.Item
			for _, item := range items {
				if item.ID == "" || len(item.ID) > 4096 || item.ParentID != directory.id || item.Size < 0 || item.Name == "" || len(item.Name) > 4096 {
					return "", errors.New("degoo: returned inconsistent folder metadata")
				}
				if _, exists := seenItems[item.ID]; exists {
					return "", errors.New("degoo: returned duplicate or cyclic folder metadata")
				}
				seenItems[item.ID] = struct{}{}
				if directory.id == folderID && !isVaultCheckInternalMarker(item) {
					rootHasContent = true
				}
				switch item.Category {
				case 2:
					if !validVaultCheckDirectoryName(cipher, item.Name) {
						return "", errors.New("degoo: returned a directory name that does not match the vault encryption settings")
					}
					if directory.depth >= vaultCheckMaxDepth {
						return "", errors.New("folder scan exceeded its directory depth limit; vault password was not verified")
					}
					if len(queue)+len(childDirectories) >= vaultCheckMaxDirs {
						return "", errors.New("folder scan exceeded its directory limit; vault password was not verified")
					}
					if _, exists := seenDirectories[item.ID]; exists {
						return "", errors.New("degoo: returned cyclic directory metadata")
					}
					seenDirectories[item.ID] = struct{}{}
					childDirectories = append(childDirectories, vaultCheckDirectory{id: item.ID, depth: directory.depth + 1})
				case 0, 6:
					if isVaultCheckInternalMarker(item) {
						continue
					}
					if !validVaultCheckFileName(cipher, item.Name) {
						return "", errors.New("degoo: returned a file name that does not match the vault encryption settings")
					}
					if item.Size == 0 {
						possiblePlaceholders = append(possiblePlaceholders, item)
						continue
					}
					if item.Size < cipher.EncryptedSize(1) {
						if item.Size == cipher.EncryptedSize(0) {
							continue
						}
						return "", errors.New("degoo: returned an encrypted object with an invalid stored size")
					}
					if len(candidates) == 0 {
						candidates = append(candidates, item)
					}
				default:
					return "", errors.New("degoo: returned an unsupported item category in the selected folder")
				}
			}
			for _, item := range possiblePlaceholders {
				verified, err := hasVaultCheckSelfMarker(ctx, client, item.ID, budget)
				if err != nil {
					return "", fmt.Errorf("inspect zero-size Degoo item: %w", err)
				}
				if !verified {
					return "", errors.New("zero-size encrypted Degoo item has no verified directory marker")
				}
				if !validVaultCheckDirectoryName(cipher, item.Name) {
					return "", errors.New("degoo: returned a directory name that does not match the vault encryption settings")
				}
				if directory.depth >= vaultCheckMaxDepth {
					return "", errors.New("folder scan exceeded its directory depth limit; vault password was not verified")
				}
				if len(queue)+len(childDirectories) >= vaultCheckMaxDirs {
					return "", errors.New("folder scan exceeded its directory limit; vault password was not verified")
				}
				if _, exists := seenDirectories[item.ID]; exists {
					return "", errors.New("degoo: returned cyclic directory metadata")
				}
				seenDirectories[item.ID] = struct{}{}
				childDirectories = append(childDirectories, vaultCheckDirectory{id: item.ID, depth: directory.depth + 1})
			}
			queue = append(queue, childDirectories...)
			if len(candidates) > 0 {
				item := candidates[0]
				fresh, err := client.GetItem(ctx, item.ID)
				if err != nil {
					return "", fmt.Errorf("read encrypted Degoo object metadata: %w", err)
				}
				if fresh.ID != item.ID || fresh.Name != item.Name || fresh.ParentID != directory.id || fresh.Category != item.Category || fresh.Size != item.Size || fresh.Size < 0 || fresh.URL == "" {
					return "", errors.New("degoo: returned inconsistent encrypted-object metadata")
				}
				stored, err := readVaultCheckRange(ctx, client, fresh, cipher.EncryptedSize(vaultCheckPlainBlock))
				if err != nil {
					return "", fmt.Errorf("read encrypted Degoo object: %w", err)
				}
				if authenticVaultBlock(cipher, stored) {
					return vaultCheckVerified, nil
				}
				return "", errors.New("encrypted Degoo content did not authenticate with this vault password")
			}
			if nextToken == "" {
				break
			}
			if _, exists := seenCursors[nextToken]; exists {
				return "", errors.New("degoo: repeated a folder pagination cursor")
			}
			seenCursors[nextToken] = struct{}{}
			pageToken = nextToken
		}
	}
	if !rootHasContent {
		return vaultCheckEmpty, nil
	}
	return "", errors.New("the selected folder is nonempty but no encrypted content verified with this vault password")
}

func listVaultCheckPage(ctx context.Context, client *api.Client, parentID, pageToken string, budget *vaultCheckBudget) ([]api.Item, string, error) {
	if budget.pages >= vaultCheckMaxPages {
		return nil, "", errors.New("folder scan exceeded its page limit; vault password was not verified")
	}
	remaining := vaultCheckMaxItems - budget.items
	if remaining <= 0 {
		return nil, "", errors.New("folder scan exceeded its item limit; vault password was not verified")
	}
	limit := min(vaultCheckPageSize, remaining)
	items, nextToken, err := client.ListDirPage(ctx, parentID, limit, pageToken)
	budget.pages++
	if err != nil {
		return nil, "", err
	}
	if len(items) > remaining {
		return nil, "", errors.New("folder scan exceeded its item limit; vault password was not verified")
	}
	budget.items += len(items)
	return items, nextToken, nil
}

func hasVaultCheckSelfMarker(ctx context.Context, client *api.Client, directoryID string, budget *vaultCheckBudget) (bool, error) {
	expectedName := vaultCheckSelfMarkerName(directoryID)
	seenIDs := make(map[string]struct{})
	seenCursors := make(map[string]struct{})
	pageToken := ""
	found := false
	for {
		items, nextToken, err := listVaultCheckPage(ctx, client, directoryID, pageToken, budget)
		if err != nil {
			return false, err
		}
		for _, item := range items {
			if item.ID == "" || len(item.ID) > 4096 || item.ParentID != directoryID || item.Size < 0 || len(item.Name) > 4096 {
				return false, errors.New("degoo: returned inconsistent placeholder metadata")
			}
			if _, exists := seenIDs[item.ID]; exists {
				return false, errors.New("degoo: returned duplicate placeholder metadata")
			}
			seenIDs[item.ID] = struct{}{}
			if item.Name != expectedName {
				continue
			}
			if found || item.Size != 0 || (item.Category != 0 && item.Category != 6) {
				return false, errors.New("degoo: returned an invalid or duplicate placeholder marker")
			}
			found = true
		}
		if nextToken == "" {
			return found, nil
		}
		if _, exists := seenCursors[nextToken]; exists {
			return false, errors.New("degoo: repeated a placeholder pagination cursor")
		}
		seenCursors[nextToken] = struct{}{}
		pageToken = nextToken
	}
}

func vaultCheckSelfMarkerName(directoryID string) string {
	markerID := sha256.Sum256([]byte(directoryID))
	return vaultCheckMarkerPrefix + hex.EncodeToString(markerID[:16])
}

func validVaultCheckFileName(cipher *crypt.Cipher, encrypted string) bool {
	if len(encrypted) > 4096 {
		return false
	}
	name, err := cipher.DecryptFileName(encrypted)
	return err == nil && validVaultCheckLeaf(name)
}

func validVaultCheckDirectoryName(cipher *crypt.Cipher, encrypted string) bool {
	if len(encrypted) > 4096 {
		return false
	}
	// DecryptDirName can log the private plaintext name for legacy versioned
	// directories, so reject that unsupported legacy form before calling it.
	if version.Match(path.Base(encrypted)) {
		return false
	}
	name, err := cipher.DecryptDirName(encrypted)
	return err == nil && validVaultCheckLeaf(name)
}

func validVaultCheckLeaf(name string) bool {
	return name != "" && len(name) <= 4096 && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func isVaultCheckInternalMarker(item api.Item) bool {
	if item.Size != 0 || (item.Category != 0 && item.Category != 6) {
		return false
	}
	for _, prefix := range []string{vaultCheckMarkerPrefix, vaultCheckRetiredPrefix} {
		if !strings.HasPrefix(item.Name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(item.Name, prefix)
		decoded, err := hex.DecodeString(suffix)
		return err == nil && len(decoded) == 16 && suffix == strings.ToLower(suffix)
	}
	return false
}

func readVaultCheckRange(ctx context.Context, client *api.Client, item api.Item, maximum int64) ([]byte, error) {
	requested := item.Size
	if requested > maximum {
		requested = maximum
	}
	if requested <= 0 {
		return nil, errors.New("remote object has an invalid size")
	}
	response, err := client.Download(ctx, item.URL, "bytes=0-"+strconv.FormatInt(requested-1, 10))
	if err != nil {
		return nil, err
	}
	if response == nil || response.Body == nil {
		return nil, errors.New("degoo: returned no ranged object body")
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, errors.New("ranged object response was incomplete")
	}
	if closeErr != nil {
		return nil, errors.New("ranged object response could not be closed safely")
	}
	if int64(len(data)) != requested {
		return nil, errors.New("ranged object response length did not match its metadata")
	}
	return data, nil
}

func authenticVaultBlock(cipher *crypt.Cipher, stored []byte) bool {
	reader, err := cipher.DecryptData(io.NopCloser(bytes.NewReader(stored)))
	if err != nil {
		return false
	}
	var first [1]byte
	_, err = io.ReadFull(reader, first[:])
	closeErr := reader.Close()
	return err == nil && closeErr == nil
}
