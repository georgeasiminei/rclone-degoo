//go:build linux

package degoo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/backend/degoo/auth"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

type checkVaultTestTokens struct{}

func (checkVaultTestTokens) Token(context.Context) (string, error) { return "generated-token", nil }
func (checkVaultTestTokens) Refresh(context.Context, string) (string, error) {
	return "generated-token", nil
}

type checkVaultPage struct {
	items          []api.Item
	next           string
	missingListing bool
}

type checkVaultTestRemote struct {
	server       *httptest.Server
	mu           sync.Mutex
	items        map[string]api.Item
	pages        map[string]map[string]checkVaultPage
	objects      map[string][]byte
	truncated    map[string]bool
	ignoreRange  map[string]bool
	graphqlError map[string]bool
	operations   []string
	mutationCall atomic.Int32
	downloadCall atomic.Int32
	rangeBytes   atomic.Int64
}

func newCheckVaultTestRemote(t *testing.T) *checkVaultTestRemote {
	t.Helper()
	remote := &checkVaultTestRemote{
		items:        make(map[string]api.Item),
		pages:        make(map[string]map[string]checkVaultPage),
		objects:      make(map[string][]byte),
		truncated:    make(map[string]bool),
		ignoreRange:  make(map[string]bool),
		graphqlError: make(map[string]bool),
	}
	remote.server = httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	t.Cleanup(remote.server.Close)
	return remote
}

func (remote *checkVaultTestRemote) setFolder(id, parentID string, category int, size int64) {
	remote.mu.Lock()
	remote.items[id] = api.Item{ID: id, Name: "private-folder-name", ParentID: parentID, Category: category, Size: size}
	remote.mu.Unlock()
}

func (remote *checkVaultTestRemote) addObject(parentID, id, encryptedName string, category int, data []byte) api.Item {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	item := api.Item{
		ID: id, Name: encryptedName, ParentID: parentID, Category: category,
		Size: int64(len(data)), URL: remote.server.URL + "/object/" + id,
	}
	remote.items[id] = item
	remote.objects[id] = append([]byte(nil), data...)
	return api.Item{ID: item.ID, Name: item.Name, ParentID: item.ParentID, Category: item.Category, Size: item.Size}
}

func (remote *checkVaultTestRemote) setPage(parentID, token string, page checkVaultPage) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.pages[parentID] == nil {
		remote.pages[parentID] = make(map[string]checkVaultPage)
	}
	remote.pages[parentID][token] = page
}

func (remote *checkVaultTestRemote) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet {
		remote.serveDownload(writer, request)
		return
	}
	var body struct {
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	remote.mu.Lock()
	remote.operations = append(remote.operations, body.OperationName)
	if strings.HasPrefix(body.OperationName, "Set") || strings.Contains(body.OperationName, "Mutation") {
		remote.mu.Unlock()
		remote.mutationCall.Add(1)
		http.Error(writer, "mutation request is forbidden in this test", http.StatusInternalServerError)
		return
	}
	if remote.graphqlError[body.OperationName] {
		remote.mu.Unlock()
		writeCheckVaultJSON(writer, map[string]any{"errors": []any{map[string]any{"message": "generated fixture error", "errorType": "GeneratedError"}}})
		return
	}
	if body.OperationName == "GetOverlay4" {
		idArgs, _ := body.Variables["ID"].(map[string]any)
		id, _ := idArgs["FileID"].(string)
		item, ok := remote.items[id]
		remote.mu.Unlock()
		if !ok {
			writeCheckVaultJSON(writer, map[string]any{"data": map[string]any{"getOverlay4": nil}})
			return
		}
		writeCheckVaultJSON(writer, map[string]any{"data": map[string]any{"getOverlay4": item}})
		return
	}
	if body.OperationName == "GetFileChildren5" {
		parentID, _ := body.Variables["ParentID"].(string)
		token, _ := body.Variables["NextToken"].(string)
		page, ok := remote.pages[parentID][token]
		remote.mu.Unlock()
		if !ok {
			page.items = []api.Item{}
		}
		if page.missingListing {
			writeCheckVaultJSON(writer, map[string]any{"data": map[string]any{}})
			return
		}
		if page.items == nil {
			page.items = []api.Item{}
		}
		writeCheckVaultJSON(writer, map[string]any{"data": map[string]any{"getFileChildren5": map[string]any{"Items": page.items, "NextToken": page.next}}})
		return
	}
	remote.mu.Unlock()
	http.Error(writer, "unexpected operation", http.StatusBadRequest)
}

func (remote *checkVaultTestRemote) serveDownload(writer http.ResponseWriter, request *http.Request) {
	remote.downloadCall.Add(1)
	remote.mu.Lock()
	id := strings.TrimPrefix(request.URL.Path, "/object/")
	data := append([]byte(nil), remote.objects[id]...)
	truncated := remote.truncated[id]
	ignoreRange := remote.ignoreRange[id]
	remote.mu.Unlock()
	var start, end int64
	if _, err := fmt.Sscanf(request.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start != 0 || end < start || end >= int64(len(data)) {
		http.Error(writer, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	content := data[start : end+1]
	if ignoreRange {
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(data)
		return
	}
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	writer.Header().Set("Content-Length", strconv.Itoa(len(content)))
	writer.WriteHeader(http.StatusPartialContent)
	if truncated && len(content) > 0 {
		content = content[:len(content)-1]
	}
	_, _ = writer.Write(content)
	remote.rangeBytes.Add(int64(len(content)))
}

func writeCheckVaultJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

func checkVaultTestFiles(t *testing.T, password, salt string) (authFile, vaultFile string) {
	t.Helper()
	private := filepath.Join(t.TempDir(), "private")
	require.NoError(t, os.Mkdir(private, 0o700))
	authFile = filepath.Join(private, "auth.json")
	require.NoError(t, auth.WritePrivateFile(authFile, []byte(`{"token":"generated-test-token","refresh_token":"generated-test-refresh-token"}`)))
	vaultFile = filepath.Join(private, "candidate-vault.json")
	data, err := json.Marshal(VaultConfig{
		Version: 1, Remote: "degoo-check-test:", Password: password, PasswordSalt: salt,
		CacheDir: filepath.Join(private, "cache"), ChunkSize: 1024, MaxObjectSize: 4096,
	})
	require.NoError(t, err)
	require.NoError(t, auth.WritePrivateFile(vaultFile, append(data, '\n')))
	return authFile, vaultFile
}

func checkVaultAPIFactory(t *testing.T, remote *checkVaultTestRemote, authFile, vaultFile string) setupAPIFunc {
	t.Helper()
	return func(ctx context.Context, m configmap.Getter) (*api.Client, error) {
		authPath, _ := m.Get("auth_file")
		vaultPath, _ := m.Get("credentials_file")
		require.Equal(t, authFile, authPath)
		require.Equal(t, vaultFile, vaultPath)
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline, "remote verification must have a total time bound")
		return api.New(remote.server.Client(), checkVaultTestTokens{}, remote.server.URL, "generated-test-api-key"), nil
	}
}

func runCheckVaultTestCommand(ctx context.Context, t *testing.T, authFile, vaultFile, folderID string, makeAPI setupAPIFunc) ([]byte, error) {
	t.Helper()
	root := &cobra.Command{Use: "degoo", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(newCheckVaultCommand(makeAPI))
	root.SetArgs([]string{"check-vault", authFile, "--vault-config", vaultFile, "--folder-id", folderID})
	root.SetContext(ctx)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	return output.Bytes(), err
}

func encryptCheckVaultFixture(t *testing.T, password, salt string, plaintext []byte) []byte {
	t.Helper()
	cipher := checkVaultFixtureCipher(t, password, salt)
	encrypted, err := cipher.EncryptData(bytes.NewReader(plaintext))
	require.NoError(t, err)
	stored, err := io.ReadAll(encrypted)
	require.NoError(t, err)
	return stored
}

func checkVaultFixtureCipher(t *testing.T, password, salt string) *crypt.Cipher {
	t.Helper()
	encodedPassword, err := obscure.Obscure(password)
	require.NoError(t, err)
	encodedSalt, err := obscure.Obscure(salt)
	require.NoError(t, err)
	cipher, err := crypt.NewCipher(configmap.Simple{
		"password": encodedPassword, "password2": encodedSalt,
		"filename_encryption": "standard", "filename_encoding": "base32",
		"directory_name_encryption": "true", "no_data_encryption": "false", "suffix": ".bin",
	})
	require.NoError(t, err)
	return cipher
}

const (
	checkVaultRootID = "generated-dedicated-folder"
	checkVaultParent = "generated-account-parent"
	checkVaultPass   = "generated-vault-password"
	checkVaultSalt   = "generated-independent-salt"
)

func prepareCheckVaultCommand(t *testing.T, remote *checkVaultTestRemote, password, salt string) (authFile, vaultFile string, makeAPI setupAPIFunc) {
	t.Helper()
	authFile, vaultFile = checkVaultTestFiles(t, password, salt)
	remote.setFolder(checkVaultRootID, checkVaultParent, 2, 0)
	return authFile, vaultFile, checkVaultAPIFactory(t, remote, authFile, vaultFile)
}

func checkVaultListedItem(item api.Item) []api.Item { return []api.Item{item} }

func TestCheckVaultClassifiesEmptyObservationOrAuthenticatedFolder(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{}})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.NoError(t, err)
		require.JSONEq(t, `{"status":"empty"}`, string(output))
		require.Zero(t, remote.downloadCall.Load())
		require.Zero(t, remote.mutationCall.Load())
	})

	t.Run("generated ciphertext with password and custom salt", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
		name := cipher.EncryptFileName("generated fixture.txt")
		stored := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("generated encrypted fixture"))
		listed := remote.addObject(checkVaultRootID, "generated-file-id", name, 6, stored)
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: checkVaultListedItem(listed)})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.NoError(t, err)
		require.JSONEq(t, `{"status":"verified"}`, string(output))
		require.EqualValues(t, 1, remote.downloadCall.Load())
		require.Zero(t, remote.mutationCall.Load())
	})
}

func TestCheckVaultRejectsWrongKeySaltAndBitFlippedContent(t *testing.T) {
	tests := []struct {
		name     string
		password string
		salt     string
		mutate   func([]byte) []byte
	}{
		{name: "wrong password", password: "wrong generated password", salt: checkVaultSalt},
		{name: "wrong salt", password: checkVaultPass, salt: "wrong generated salt"},
		{name: "bit flipped authenticated block", password: checkVaultPass, salt: checkVaultSalt, mutate: func(data []byte) []byte {
			data = append([]byte(nil), data...)
			data[32+16] ^= 0x80
			return data
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := newCheckVaultTestRemote(t)
			authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, test.password, test.salt)
			cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
			stored := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("authenticated generated content"))
			if test.mutate != nil {
				stored = test.mutate(stored)
			}
			listed := remote.addObject(checkVaultRootID, "generated-file-id", cipher.EncryptFileName("not emitted private name.txt"), 0, stored)
			remote.setPage(checkVaultRootID, "", checkVaultPage{items: checkVaultListedItem(listed)})
			output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
			require.Error(t, err)
			require.Empty(t, output)
			require.Zero(t, remote.mutationCall.Load())
		})
	}

	t.Run("damaged first object is not masked by a later healthy object", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
		corrupt := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("generated corrupt first object"))
		corrupt[32+16] ^= 0x80
		healthy := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("generated healthy second object"))
		first := remote.addObject(checkVaultRootID, "first-generated-file", cipher.EncryptFileName("a-first.txt"), 0, corrupt)
		second := remote.addObject(checkVaultRootID, "second-generated-file", cipher.EncryptFileName("b-second.txt"), 0, healthy)
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{first, second}})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.EqualValues(t, 1, remote.downloadCall.Load(), "a later file must not mask the first damaged encrypted object")
		require.NotContains(t, err.Error(), "a-first.txt")
		require.NotContains(t, err.Error(), "b-second.txt")
	})
}

func TestCheckVaultValidatesEveryPageNameBeforeDownloading(t *testing.T) {
	remote := newCheckVaultTestRemote(t)
	authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
	cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
	stored := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("generated encrypted fixture"))
	valid := remote.addObject(checkVaultRootID, "valid-first", cipher.EncryptFileName("generated private name.txt"), 0, stored)
	plaintext := remote.addObject(checkVaultRootID, "plaintext-second", "plaintext-private-name.txt", 0, []byte("plaintext"))
	remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{valid, plaintext}})
	output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
	require.Error(t, err)
	require.Empty(t, output)
	require.Zero(t, remote.downloadCall.Load(), "the valid first item must not mask the invalid sibling")
	require.NotContains(t, err.Error(), "plaintext-private-name")
	require.Zero(t, remote.mutationCall.Load())
}

func TestCheckVaultRejectsPlaintextAndFreshMetadataMismatch(t *testing.T) {
	t.Run("plaintext object", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		listed := remote.addObject(checkVaultRootID, "plaintext-file", "plaintext.txt", 0, []byte("not encrypted"))
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: checkVaultListedItem(listed)})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.Zero(t, remote.downloadCall.Load())
	})

	t.Run("fresh name mismatch", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
		stored := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("generated encrypted fixture"))
		listed := remote.addObject(checkVaultRootID, "generated-file-id", cipher.EncryptFileName("listing-name.txt"), 0, stored)
		remote.mu.Lock()
		fresh := remote.items[listed.ID]
		fresh.Name = cipher.EncryptFileName("different-fresh-name.txt")
		remote.items[listed.ID] = fresh
		remote.mu.Unlock()
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: checkVaultListedItem(listed)})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.NotContains(t, err.Error(), "listing-name")
	})
}

func TestCheckVaultDoesNotSkipMalformedShortObjectsBeforeHealthySibling(t *testing.T) {
	for _, size := range []int64{0, 1, 31, 33, 48} {
		t.Run(fmt.Sprintf("stored size %d", size), func(t *testing.T) {
			remote := newCheckVaultTestRemote(t)
			authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
			cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
			broken := api.Item{
				ID: "generated-short-item", Name: cipher.EncryptFileName("short.dat"),
				ParentID: checkVaultRootID, Category: 0, Size: size,
			}
			healthyData := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("healthy sibling"))
			healthy := remote.addObject(checkVaultRootID, "healthy-after-short", cipher.EncryptFileName("healthy.txt"), 0, healthyData)
			remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{broken, healthy}})
			output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
			require.Error(t, err)
			require.Empty(t, output)
			require.Zero(t, remote.downloadCall.Load(), "the later healthy sibling must not mask malformed short content")
		})
	}
}

func TestCheckVaultAllowsAWellFormedEncryptedEmptyFileAlongsideVerifiableContent(t *testing.T) {
	remote := newCheckVaultTestRemote(t)
	authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
	cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
	empty := api.Item{
		ID: "generated-empty-encrypted-file", Name: cipher.EncryptFileName("empty.txt"),
		ParentID: checkVaultRootID, Category: 0, Size: cipher.EncryptedSize(0),
	}
	data := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("nonempty sibling"))
	healthy := remote.addObject(checkVaultRootID, "healthy-after-empty", cipher.EncryptFileName("healthy.txt"), 6, data)
	remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{empty, healthy}})
	output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
	require.NoError(t, err)
	require.JSONEq(t, `{"status":"verified"}`, string(output))
	require.EqualValues(t, 1, remote.downloadCall.Load())
}

func TestVaultCheckLeafUsesBackendPathByteRules(t *testing.T) {
	require.True(t, validVaultCheckLeaf("back\\slash"), "backslash is an ordinary Linux filename byte")
	require.True(t, validVaultCheckLeaf(string([]byte{0xff})), "the backend accepts non-UTF-8 filename bytes")
	for _, invalid := range []string{"", ".", "..", "a/b", "a\x00b", strings.Repeat("x", 4097)} {
		require.False(t, validVaultCheckLeaf(invalid), "invalid logical leaf %q", invalid)
	}
}

func TestCheckVaultTraversesEncryptedDirectoryAndSkipsExactInternalMarkers(t *testing.T) {
	t.Run("encrypted nested directory", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
		directory := api.Item{ID: "generated-nested-id", Name: cipher.EncryptDirName("generated nested"), ParentID: checkVaultRootID, Category: 2}
		remote.items[directory.ID] = directory
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{directory}})
		stored := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("generated content in subdirectory"))
		file := remote.addObject(directory.ID, "nested-file-id", cipher.EncryptFileName("nested.txt"), 6, stored)
		remote.setPage(directory.ID, "", checkVaultPage{items: []api.Item{file}})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.NoError(t, err)
		require.JSONEq(t, `{"status":"verified"}`, string(output))
	})

	t.Run("exact empty marker", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		marker := api.Item{ID: "generated-marker-id", Name: vaultCheckMarkerPrefix + "00112233445566778899aabbccddeeff", ParentID: checkVaultRootID, Category: 6, Size: 0}
		remote.items[marker.ID] = marker
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{marker}})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.NoError(t, err)
		require.JSONEq(t, `{"status":"empty"}`, string(output))
		require.Zero(t, remote.downloadCall.Load())
	})

	t.Run("malformed marker is not hidden", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		marker := api.Item{ID: "generated-marker-id", Name: vaultCheckMarkerPrefix + "not-hex", ParentID: checkVaultRootID, Category: 6, Size: 0}
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{marker}})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
	})
}

func TestCheckVaultVerifiesSelfMarkedDirectoryPlaceholders(t *testing.T) {
	t.Run("fresh unpromoted root placeholder is empty", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		remote.setFolder(checkVaultRootID, checkVaultParent, 6, 0)
		marker := api.Item{
			ID: "generated-root-marker-id", Name: vaultCheckSelfMarkerName(checkVaultRootID),
			ParentID: checkVaultRootID, Category: 0, Size: 0,
		}
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{marker}})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.NoError(t, err)
		require.JSONEq(t, `{"status":"empty"}`, string(output))
		require.Zero(t, remote.mutationCall.Load())
	})

	t.Run("nested placeholder exposes encrypted files", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
		placeholder := api.Item{
			ID: "generated-placeholder-id", Name: cipher.EncryptDirName("nested placeholder"),
			ParentID: checkVaultRootID, Category: 6, Size: 0,
		}
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{placeholder}})
		marker := api.Item{
			ID: "generated-placeholder-marker", Name: vaultCheckSelfMarkerName(placeholder.ID),
			ParentID: placeholder.ID, Category: 6, Size: 0,
		}
		stored := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("encrypted in placeholder"))
		file := remote.addObject(placeholder.ID, "nested-placeholder-file", cipher.EncryptFileName("payload.txt"), 0, stored)
		remote.setPage(placeholder.ID, "", checkVaultPage{items: []api.Item{marker, file}})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.NoError(t, err)
		require.JSONEq(t, `{"status":"verified"}`, string(output))
		require.EqualValues(t, 1, remote.downloadCall.Load())
		require.Zero(t, remote.mutationCall.Load())
	})

	t.Run("missing marker does not establish a placeholder", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		remote.setFolder(checkVaultRootID, checkVaultParent, 0, 0)
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{}})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.Contains(t, err.Error(), "self marker")
		require.Zero(t, remote.downloadCall.Load())
		require.Zero(t, remote.mutationCall.Load())
	})

	t.Run("nested placeholder without marker is not traversed", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
		placeholder := api.Item{
			ID: "generated-unmarked-placeholder", Name: cipher.EncryptDirName("unmarked placeholder"),
			ParentID: checkVaultRootID, Category: 0, Size: 0,
		}
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{placeholder}})
		stored := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("must not infer directory without marker"))
		file := remote.addObject(placeholder.ID, "unmarked-nested-file", cipher.EncryptFileName("hidden.txt"), 6, stored)
		remote.setPage(placeholder.ID, "", checkVaultPage{items: []api.Item{file}})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.Zero(t, remote.downloadCall.Load())
		require.Zero(t, remote.mutationCall.Load())
	})
}

func TestCheckVaultRejectsMalformedRootPaginationAndAPIResponses(t *testing.T) {
	t.Run("invalid selected root metadata", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			parentID string
			category int
			size     int64
		}{
			{name: "account root", parentID: "0", category: 2},
			{name: "not a directory", parentID: checkVaultParent, category: 1},
			{name: "negative size", parentID: checkVaultParent, category: 2, size: -1},
		} {
			t.Run(test.name, func(t *testing.T) {
				remote := newCheckVaultTestRemote(t)
				authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
				remote.setFolder(checkVaultRootID, test.parentID, test.category, test.size)
				output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
				require.Error(t, err)
				require.Empty(t, output)
				require.Equal(t, 1, remote.operationsCount(), "only the selected item's metadata should be read")
			})
		}
	})

	t.Run("repeated cursor", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{}, next: "repeat"})
		remote.setPage(checkVaultRootID, "repeat", checkVaultPage{items: []api.Item{}, next: "repeat"})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.Contains(t, err.Error(), "repeated")
	})

	t.Run("API error", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		remote.graphqlError["GetFileChildren5"] = true
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.Zero(t, remote.mutationCall.Load())
	})
}

func TestCheckVaultRejectsIgnoredOrTruncatedRangeAndHonorsCancellation(t *testing.T) {
	for _, mode := range []string{"ignored range", "short body"} {
		t.Run(mode, func(t *testing.T) {
			remote := newCheckVaultTestRemote(t)
			authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
			cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
			stored := encryptCheckVaultFixture(t, checkVaultPass, checkVaultSalt, []byte("generated encrypted fixture"))
			listed := remote.addObject(checkVaultRootID, "generated-file-id", cipher.EncryptFileName("file.txt"), 0, stored)
			remote.setPage(checkVaultRootID, "", checkVaultPage{items: []api.Item{listed}})
			if mode == "ignored range" {
				remote.ignoreRange[listed.ID] = true
			} else {
				remote.truncated[listed.ID] = true
			}
			output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
			require.Error(t, err)
			require.Empty(t, output)
			require.Zero(t, remote.mutationCall.Load())
		})
	}

	t.Run("cancelled before remote call", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		output, err := runCheckVaultTestCommand(ctx, t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.Zero(t, remote.operationsCount())
	})
}

func TestCheckVaultEnforcesTraversalLimits(t *testing.T) {
	t.Run("directory fanout limit", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		cipher := checkVaultFixtureCipher(t, checkVaultPass, checkVaultSalt)
		items := make([]api.Item, 0, vaultCheckMaxDirs+1)
		for index := 0; index <= vaultCheckMaxDirs; index++ {
			items = append(items, api.Item{
				ID: fmt.Sprintf("directory-%d", index), Name: cipher.EncryptDirName(fmt.Sprintf("generated-dir-%d", index)),
				ParentID: checkVaultRootID, Category: 2,
			})
		}
		remote.setPage(checkVaultRootID, "", checkVaultPage{items: items})
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.Contains(t, err.Error(), "directory limit")
		require.Equal(t, 2, remote.operationsCount(), "root metadata and one listing only; child directories are not traversed after overflow")
	})

	t.Run("page limit", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		for page := 0; page < vaultCheckMaxPages; page++ {
			token := ""
			if page != 0 {
				token = fmt.Sprintf("page-%d", page)
			}
			next := fmt.Sprintf("page-%d", page+1)
			remote.setPage(checkVaultRootID, token, checkVaultPage{items: []api.Item{}, next: next})
		}
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.Contains(t, err.Error(), "page limit")
		require.Zero(t, remote.mutationCall.Load())
	})

	t.Run("item limit", func(t *testing.T) {
		remote := newCheckVaultTestRemote(t)
		authFile, vaultFile, makeAPI := prepareCheckVaultCommand(t, remote, checkVaultPass, checkVaultSalt)
		for page := 0; page < vaultCheckMaxItems/vaultCheckPageSize; page++ {
			items := make([]api.Item, 0, vaultCheckPageSize)
			for offset := 0; offset < vaultCheckPageSize; offset++ {
				index := page*vaultCheckPageSize + offset + 1
				items = append(items, api.Item{
					ID:       fmt.Sprintf("marker-%d", index),
					Name:     vaultCheckMarkerPrefix + fmt.Sprintf("%032x", index),
					ParentID: checkVaultRootID, Category: 6, Size: 0,
				})
			}
			token := ""
			if page != 0 {
				token = fmt.Sprintf("page-%d", page)
			}
			remote.setPage(checkVaultRootID, token, checkVaultPage{items: items, next: fmt.Sprintf("page-%d", page+1)})
		}
		output, err := runCheckVaultTestCommand(context.Background(), t, authFile, vaultFile, checkVaultRootID, makeAPI)
		require.Error(t, err)
		require.Empty(t, output)
		require.Contains(t, err.Error(), "item limit")
	})

}

func (remote *checkVaultTestRemote) operationsCount() int {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	return len(remote.operations)
}
