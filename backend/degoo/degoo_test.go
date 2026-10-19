package degoo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/backend/degoo/auth"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
)

type mockItem struct {
	id           string
	name         string
	parentID     string
	category     int
	size         int64
	created      time.Time
	content      []byte
	visibleAfter time.Time
}

type mockDegoo struct {
	mu                           sync.Mutex
	items                        map[string]mockItem
	uploads                      map[string][]byte
	downloadCalls                int
	rangeDownloadCalls           int
	expireNextDownload           bool
	nextID                       int
	failRegister                 bool
	nullRegisterAcknowledgement  bool
	falseRegisterAcknowledgement bool
	applyFalseRegisterAck        bool
	registrationVisibilityLag    time.Duration
	registerCalls                int
	registerRequestCalls         int
	authorizationRequestCalls    int
	authorizationRows            int
	failWriteAuthName            string
	failFirstWriteAuth           bool
	authMappingErrors            int
	authRequestSeen              chan int
	authRequestRelease           chan struct{}
	authRequestCanceled          chan struct{}
	blockUpload                  bool
	uploadStarted                chan struct{}
	uploadRelease                chan struct{}
	blockListing                 bool
	blockListingPage2            bool
	injectTargetOnDownload       bool
	injectedTargetName           string
	injectedTargetContent        []byte
	listPage2Started             chan struct{}
	listNextToken                string
	blockGetItem                 bool
	listRetryAfter               string
	listStatusCode               int
	listCalls                    int
	listPageSize                 int
	getItemCalls                 int
	failInitialMkdirAck          bool
	corruptUpload                bool
	registeredSizeDelta          int64
	promoteOnMarker              bool
	initialPlaceholderCategory   int
	renameRoutesViaAlias         bool
	listLagForName               string
	listLagID                    string
	listLagOldName               string
	listLagFinalName             string
	listLagResponses             int
	listLagRemaining             int
	getItemSawFinalName          bool
	overlayParentZero            bool
	backupID                     string
	backupDeletedBeforeVisible   bool
	deleteCalls                  []deleteCall
	failDeleteIDs                map[string]bool
	registerSeen                 chan struct{}
}

type deleteCall struct {
	id        string
	permanent bool
}

func newMockDegoo() *mockDegoo {
	return &mockDegoo{
		items:                      map[string]mockItem{"vault": {id: "vault", name: "vault", category: 2}},
		uploads:                    make(map[string][]byte),
		initialPlaceholderCategory: 6,
	}
}

func closeTestBody(t *testing.T, body io.ReadCloser) {
	t.Helper()
	if err := body.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func (mock *mockDegoo) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/gcs" {
		mock.upload(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/download/") {
		mock.download(writer, request)
		return
	}
	mock.graphql(writer, request)
}

func (mock *mockDegoo) upload(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		http.Error(writer, "invalid multipart upload", http.StatusBadRequest)
		return
	}
	file, header, err := request.FormFile("file")
	if err != nil {
		http.Error(writer, "missing file part", http.StatusBadRequest)
		return
	}
	content, err := io.ReadAll(file)
	closeErr := file.Close()
	if err != nil {
		http.Error(writer, "read upload", http.StatusInternalServerError)
		return
	}
	if closeErr != nil {
		http.Error(writer, "close upload", http.StatusInternalServerError)
		return
	}
	mock.mu.Lock()
	blockUpload := mock.blockUpload
	started, release := mock.uploadStarted, mock.uploadRelease
	mock.mu.Unlock()
	if blockUpload {
		if started != nil {
			select {
			case started <- struct{}{}:
			default:
			}
		}
		select {
		case <-request.Context().Done():
			return
		case <-release:
		}
	}
	if got, want := request.FormValue("auth-name"), header.Filename; got != want {
		mock.mu.Lock()
		mock.authMappingErrors++
		mock.mu.Unlock()
		http.Error(writer, "authorization row did not match upload", http.StatusBadRequest)
		return
	}
	mock.mu.Lock()
	if mock.corruptUpload && len(content) != 0 {
		content[0] ^= 0xff
	}
	mock.uploads[header.Filename] = content
	mock.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (mock *mockDegoo) download(writer http.ResponseWriter, request *http.Request) {
	id := strings.TrimPrefix(request.URL.Path, "/download/")
	rangeHeader := request.Header.Get("Range")
	mock.mu.Lock()
	mock.downloadCalls++
	if rangeHeader != "" {
		mock.rangeDownloadCalls++
	}
	if mock.expireNextDownload {
		mock.expireNextDownload = false
		mock.mu.Unlock()
		writer.WriteHeader(http.StatusForbidden)
		return
	}
	item, ok := mock.items[id]
	content := append([]byte(nil), item.content...)
	if ok && mock.injectTargetOnDownload {
		mock.injectTargetOnDownload = false
		mock.items["external-target"] = mockItem{
			id: "external-target", name: mock.injectedTargetName, parentID: item.parentID,
			category: 6, size: int64(len(mock.injectedTargetContent)), content: append([]byte(nil), mock.injectedTargetContent...),
		}
	}
	mock.mu.Unlock()
	if !ok {
		http.NotFound(writer, request)
		return
	}
	if rangeHeader != "" {
		rangeOption, err := fs.ParseRangeOption(rangeHeader)
		if err != nil {
			http.Error(writer, "invalid range", http.StatusBadRequest)
			return
		}
		offset, limit := rangeOption.Decode(int64(len(content)))
		if offset < 0 || offset >= int64(len(content)) || limit == 0 {
			writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(content)))
			writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if limit < 0 || offset+limit > int64(len(content)) {
			limit = int64(len(content)) - offset
		}
		end := offset + limit - 1
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end, len(content)))
		writer.Header().Set("Content-Length", strconv.FormatInt(limit, 10))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(content[offset : end+1])
		return
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(content)))
	_, _ = writer.Write(content)
}

func (mock *mockDegoo) graphql(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Operation string         `json:"operationName"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		http.Error(writer, "invalid GraphQL request", http.StatusBadRequest)
		return
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	var data any
	switch input.Operation {
	case "GetOverlay4":
		mock.getItemCalls++
		if mock.blockGetItem {
			<-request.Context().Done()
			return
		}
		idObject, _ := input.Variables["ID"].(map[string]any)
		id, _ := idObject["FileID"].(string)
		item, ok := mock.items[id]
		if !ok {
			data = map[string]any{"getOverlay4": nil}
		} else {
			if id == mock.listLagID && item.name == mock.listLagFinalName {
				mock.getItemSawFinalName = true
			}
			overlayItem := item
			if mock.overlayParentZero {
				overlayItem.parentID = "0"
			}
			data = map[string]any{"getOverlay4": mock.itemJSON(overlayItem, request)}
		}
	case "GetFileChildren5":
		mock.listCalls++
		nextToken, _ := input.Variables["NextToken"].(string)
		if nextToken != "" && mock.blockListingPage2 {
			if mock.listPage2Started != nil {
				select {
				case mock.listPage2Started <- struct{}{}:
				default:
				}
			}
			<-request.Context().Done()
			return
		}
		if mock.blockListing {
			<-request.Context().Done()
			return
		}
		if mock.listRetryAfter != "" {
			writer.Header().Set("Retry-After", mock.listRetryAfter)
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if mock.listStatusCode != 0 {
			writer.WriteHeader(mock.listStatusCode)
			return
		}
		parentID, _ := input.Variables["ParentID"].(string)
		items := make([]map[string]any, 0)
		for _, item := range mock.items {
			if item.parentID == parentID && !time.Now().Before(item.visibleAfter) {
				listedItem := item
				if item.id == mock.listLagID && mock.listLagRemaining > 0 {
					listedItem.name = mock.listLagOldName
					mock.listLagRemaining--
				}
				items = append(items, mock.itemJSON(listedItem, request))
			}
		}
		responseNextToken := ""
		if mock.listPageSize > 0 {
			sort.Slice(items, func(i, j int) bool {
				return items[i]["ID"].(string) < items[j]["ID"].(string)
			})
			offset := 0
			if nextToken != "" {
				var err error
				offset, err = strconv.Atoi(nextToken)
				if err != nil || offset < 0 || offset > len(items) {
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			end := offset + mock.listPageSize
			if end > len(items) {
				end = len(items)
			}
			if end < len(items) {
				responseNextToken = strconv.Itoa(end)
			}
			items = items[offset:end]
		} else if nextToken == "" {
			responseNextToken = mock.listNextToken
		}
		data = map[string]any{"getFileChildren5": map[string]any{"Items": items, "NextToken": responseNextToken}}
	case "GetBucketWriteAuth4":
		infos, _ := input.Variables["StorageUploadInfos"].([]any)
		mock.authorizationRequestCalls++
		mock.authorizationRows += len(infos)
		if mock.authRequestSeen != nil {
			select {
			case mock.authRequestSeen <- len(infos):
			default:
			}
		}
		if mock.authRequestRelease != nil {
			select {
			case <-request.Context().Done():
				if mock.authRequestCanceled != nil {
					select {
					case mock.authRequestCanceled <- struct{}{}:
					default:
					}
				}
				return
			case <-mock.authRequestRelease:
			}
		}
		results := make([]map[string]any, 0, len(infos))
		for i, rawInfo := range infos {
			info, _ := rawInfo.(map[string]any)
			name, _ := info["FileName"].(string)
			if name == mock.failWriteAuthName || (mock.failFirstWriteAuth && i == 0) {
				results = append(results, map[string]any{"Error": "simulated authorization rejection"})
				continue
			}
			results = append(results, map[string]any{"AuthData": map[string]any{
				"PolicyBase64": "policy", "Signature": "signature", "BaseURL": "http://" + request.Host + "/gcs",
				"KeyPrefix": "", "AccessKey": map[string]string{"Key": "key", "Value": "value"},
				"AdditionalBody": []map[string]string{{"Key": "auth-name", "Value": name}},
			}})
		}
		data = map[string]any{"getBucketWriteAuth4": results}
	case "SetUploadFile3":
		fileInfos, _ := input.Variables["FileInfos"].([]any)
		mock.registerRequestCalls++
		if len(fileInfos) == 0 {
			writeTestJSON(writer, map[string]any{"errors": []any{map[string]string{"message": "no file infos"}}})
			return
		}
		if checksum, _ := fileInfos[0].(map[string]any)["Checksum"].(string); checksum != "" && mock.failRegister {
			if mock.registerSeen != nil {
				select {
				case mock.registerSeen <- struct{}{}:
				default:
				}
			}
			writeTestJSON(writer, map[string]any{"errors": []any{map[string]string{"message": "simulated registration rejection"}}})
			return
		}
		if checksum, _ := fileInfos[0].(map[string]any)["Checksum"].(string); checksum != "" && mock.falseRegisterAcknowledgement && !mock.applyFalseRegisterAck {
			mock.registerCalls += len(fileInfos)
			data = map[string]any{"setUploadFile3": false}
			break
		}
		for _, rawInfo := range fileInfos {
			info, _ := rawInfo.(map[string]any)
			name, _ := info["Name"].(string)
			parentID, _ := info["ParentID"].(string)
			checksum, _ := info["Checksum"].(string)
			mock.nextID++
			id := fmt.Sprintf("item-%d", mock.nextID)
			size, _ := strconv.ParseInt(fmt.Sprint(info["Size"]), 10, 64)
			createdMS, _ := strconv.ParseInt(fmt.Sprint(info["CreationTime"]), 10, 64)
			category := mock.initialPlaceholderCategory
			if strings.HasPrefix(name, directoryMarkerPrefix) {
				category = 6
			}
			item := mockItem{id: id, name: name, parentID: parentID, category: category, size: size, created: time.UnixMilli(createdMS)}
			if checksum != "" {
				mock.registerCalls++
				item.size += mock.registeredSizeDelta
				item.content = append([]byte(nil), mock.uploads[name]...)
				item.visibleAfter = time.Now().Add(mock.registrationVisibilityLag)
			} else if mock.promoteOnMarker && strings.HasPrefix(name, directoryMarkerPrefix) {
				placeholder, ok := mock.items[parentID]
				if ok {
					placeholder.category = 0
					mock.items[parentID] = placeholder
					mock.nextID++
					promotedID := fmt.Sprintf("promoted-%d", mock.nextID)
					mock.items[promotedID] = mockItem{
						id: promotedID, name: placeholder.name, parentID: placeholder.parentID, category: 2,
					}
					item.parentID = promotedID
				}
			}
			mock.items[id] = item
			if checksum == "" && mock.failInitialMkdirAck && !strings.HasPrefix(name, directoryMarkerPrefix) {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		if _, checksum := fileInfos[0].(map[string]any)["Checksum"].(string); !checksum {
			data = map[string]any{"setUploadFile3": true}
		} else if mock.falseRegisterAcknowledgement {
			data = map[string]any{"setUploadFile3": false}
		} else if mock.nullRegisterAcknowledgement {
			data = map[string]any{"setUploadFile3": nil}
		} else {
			data = map[string]any{"setUploadFile3": "OK"}
		}
	case "SetRenameFile":
		rows := input.Variables["FileRenames"].([]any)
		row := rows[0].(map[string]any)
		id, _ := row["ID"].(string)
		name, _ := row["NewName"].(string)
		item := mock.items[id]
		oldName := item.name
		item.name = name
		if mock.renameRoutesViaAlias && !isInternalName(name) {
			parent, ok := mock.items[item.parentID]
			if ok && parent.category == 2 {
				for _, sibling := range mock.items {
					if sibling.id != parent.id && sibling.name == parent.name && sibling.parentID == parent.parentID && sibling.size == 0 && (sibling.category == 0 || sibling.category == 6) {
						item.parentID = sibling.id
						break
					}
				}
			}
		}
		mock.items[id] = item
		if strings.HasPrefix(name, backupPrefix) {
			mock.backupID = id
		}
		if name == mock.listLagForName {
			mock.listLagID = id
			mock.listLagOldName = oldName
			mock.listLagFinalName = name
			mock.listLagRemaining = mock.listLagResponses
		}
		data = map[string]any{"setRenameFile": "OK"}
	case "SetMoveFile":
		ids := input.Variables["FileIDs"].([]any)
		id, _ := ids[0].(string)
		parentID, _ := input.Variables["NewParentID"].(string)
		item := mock.items[id]
		item.parentID = parentID
		mock.items[id] = item
		data = map[string]any{"setMoveFile": "OK"}
	case "SetDeleteFile5":
		ids := input.Variables["IDs"].([]any)
		row := ids[0].(map[string]any)
		id, _ := row["FileID"].(string)
		permanent, _ := input.Variables["IsInRecycleBin"].(bool)
		mock.deleteCalls = append(mock.deleteCalls, deleteCall{id: id, permanent: permanent})
		if mock.failDeleteIDs[id] {
			writeTestJSON(writer, map[string]any{"errors": []any{map[string]string{"message": "simulated delete rejection"}}})
			return
		}
		if id == mock.backupID && mock.listLagRemaining > 0 {
			mock.backupDeletedBeforeVisible = true
		}
		delete(mock.items, id)
		data = map[string]any{"setDeleteFile5": "OK"}
	default:
		http.Error(writer, "unknown operation", http.StatusBadRequest)
		return
	}
	writeTestJSON(writer, map[string]any{"data": data})
}

func (mock *mockDegoo) itemJSON(item mockItem, request *http.Request) map[string]any {
	url := ""
	if item.category == 6 {
		url = "http://" + request.Host + "/download/" + item.id
	}
	modified := int64(0)
	created := int64(0)
	if !item.created.IsZero() {
		modified = item.created.UnixMilli()
		created = modified
	}
	return map[string]any{
		"ID": item.id, "Name": item.name, "ParentID": item.parentID, "Category": item.category,
		"Size": item.size, "URL": url, "LastModificationTime": modified, "CreationTime": created,
	}
}

func writeTestJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

func newTestFs(t *testing.T, mock *mockDegoo) (*Fs, *httptest.Server) {
	t.Helper()
	f, server, err := newTestFsAtRoot(t, mock, "")
	if err != nil {
		server.Close()
		t.Fatalf("NewFs: %v", err)
	}
	return f.(*Fs), server
}

func newTestFsAtRoot(t *testing.T, mock *mockDegoo, root string) (fs.Fs, *httptest.Server, error) {
	return newTestFsAtRootID(t, mock, root, "vault")
}

func newTestFsAtRootID(t *testing.T, mock *mockDegoo, root, rootID string, extras ...configmap.Simple) (fs.Fs, *httptest.Server, error) {
	t.Helper()
	return newTestFsAtRootIDWithHandler(t, mock, root, rootID, extras...)
}

func newTestFsAtRootIDWithHandler(t *testing.T, handler http.Handler, root, rootID string, extras ...configmap.Simple) (fs.Fs, *httptest.Server, error) {
	t.Helper()
	server := httptest.NewServer(handler)
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
	token := "header." + claims + ".signature"
	authDir := t.TempDir()
	if err := os.Chmod(authDir, 0o700); err != nil {
		server.Close()
		t.Fatal(err)
	}
	authPath := path.Join(authDir, "auth.json")
	if err := os.WriteFile(authPath, []byte(fmt.Sprintf(`{"token":%q,"refresh_token":"test"}`, token)), 0o600); err != nil {
		server.Close()
		t.Fatal(err)
	}
	options := configmap.Simple{
		"auth_file": authPath, "root_folder_id": rootID, "api_key": "test-key",
		"api_url": server.URL, "rest_url": server.URL,
	}
	for _, extra := range extras {
		for key, value := range extra {
			options[key] = value
		}
	}
	f, err := NewFs(context.Background(), "test", root, options)
	return f, server, err
}

func TestNewFsUsesEncryptedAccountCredentialsAfterRefreshRejection(t *testing.T) {
	const (
		email    = "vault-user@example.test"
		password = "account-password"
	)
	mock := newMockDegoo()
	var loginCalls, refreshCalls atomic.Int32
	newTokenClaims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
	newToken := "header." + newTokenClaims + ".signature"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/login":
			loginCalls.Add(1)
			var input map[string]any
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Errorf("decode login request: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if input["Username"] != email || input["Password"] != password {
				t.Errorf("login received unexpected account credentials")
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeTestJSON(writer, map[string]string{"RefreshToken": "new-refresh-token"})
		case "/access-token/v2":
			refreshCalls.Add(1)
			var input map[string]string
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Errorf("decode token request: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if input["RefreshToken"] != "new-refresh-token" {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeTestJSON(writer, map[string]string{"Token": newToken, "RefreshToken": "new-refresh-token"})
		default:
			mock.ServeHTTP(writer, request)
		}
	}))
	defer server.Close()

	authDir := t.TempDir()
	if err := os.Chmod(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authPath := path.Join(authDir, "auth.json")
	vaultPath := path.Join(authDir, "vault.json")
	expiredClaims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(-time.Hour).Unix())))
	if err := auth.WritePrivateFile(authPath, []byte(fmt.Sprintf(`{"token":%q,"refresh_token":"expired-refresh-token"}`, "header."+expiredClaims+".signature"))); err != nil {
		t.Fatal(err)
	}
	vaultJSON := []byte(`{"version":1,"remote":"test:","password":"separate-vault-password","cache_dir":"/tmp/degoo-test-cache"}`)
	if err := auth.WritePrivateFile(vaultPath, vaultJSON); err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveCredentials(vaultPath, email, password); err != nil {
		t.Fatal(err)
	}
	options := configmap.Simple{
		"auth_file": authPath, "credentials_file": vaultPath, "root_folder_id": "vault", "api_key": "test-key",
		"api_url": server.URL, "rest_url": server.URL,
	}
	if _, err := NewFs(context.Background(), "test", "", options); err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	if loginCalls.Load() != 1 {
		t.Fatalf("account login calls = %d, want 1", loginCalls.Load())
	}
	if refreshCalls.Load() < 2 {
		t.Fatalf("token refresh calls = %d, want rejected refresh and login exchange", refreshCalls.Load())
	}
}

func TestUploadAndReplacementKeepPreviousDataUntilCommit(t *testing.T) {
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()
	ctx := context.Background()

	first := []byte("first encrypted version")
	obj, err := f.Put(ctx, strings.NewReader(string(first)), object.NewStaticObjectInfo("ciphertext", time.Now(), int64(len(first)), true, nil, f))
	if err != nil {
		t.Fatalf("initial Put: %v", err)
	}
	firstID := obj.(*Object).ID()

	second := []byte("second encrypted version")
	updated, err := f.Put(ctx, strings.NewReader(string(second)), object.NewStaticObjectInfo("ciphertext", time.Now(), int64(len(second)), true, nil, f))
	if err != nil {
		t.Fatalf("replacement Put: %v", err)
	}
	if updated.(*Object).ID() == firstID {
		t.Fatal("replacement reused the old provider item")
	}
	readObject := func(obj fs.Object) string {
		t.Helper()
		body, err := obj.Open(ctx)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() {
			if err := body.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		return string(data)
	}
	if got := readObject(updated); got != string(second) {
		t.Fatalf("updated content = %q, want %q", got, second)
	}
	mock.mu.Lock()
	_, oldStillExists := mock.items[firstID]
	mock.mu.Unlock()
	if oldStillExists {
		t.Fatal("prior provider item was not removed after verified publication")
	}
}

func TestRegistrationReconcilesDelayedNullAcknowledgement(t *testing.T) {
	mock := newMockDegoo()
	mock.nullRegisterAcknowledgement = true
	mock.registrationVisibilityLag = 2500 * time.Millisecond
	f, server := newTestFs(t, mock)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	content := []byte("generated encrypted payload")
	obj, err := f.Put(ctx, bytes.NewReader(content), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(content)), true, nil, f))
	if err != nil {
		t.Fatalf("Put after unacknowledged registration: %v", err)
	}
	body, err := obj.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	closeTestBody(t, body)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("readback mismatch: %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.registerCalls != 1 || len(mock.uploads) != 1 {
		t.Fatalf("registration replayed: %d registrations, %d uploads", mock.registerCalls, len(mock.uploads))
	}
}

func TestNegativeRegistrationAcknowledgementStaysPendingWhenItemIsAbsent(t *testing.T) {
	mock := newMockDegoo()
	mock.falseRegisterAcknowledgement = true
	f, server := newTestFs(t, mock)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	content := []byte("generated encrypted payload")
	start := time.Now()
	obj, err := f.Put(ctx, bytes.NewReader(content), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(content)), true, nil, f))
	if obj != nil {
		if published, ok := obj.(*Object); !ok || published != nil {
			t.Fatalf("Put returned object %v after a negative registration acknowledgement", obj)
		}
	}
	if err == nil || !errors.Is(err, api.ErrNegativeAcknowledgement) {
		t.Fatalf("Put error = %v, want pending negative acknowledgement", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("negative acknowledgement lookup exceeded its bounded timeout: %v", elapsed)
	}
	if _, err := f.NewObject(ctx, "cipher"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("NewObject after negative acknowledgement = %v, want object not found", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.registerCalls != 1 || len(mock.uploads) != 1 || len(mock.items) != 1 {
		t.Fatalf("negative acknowledgement state = %d registrations, %d uploads, %d metadata items; want 1, 1, 1", mock.registerCalls, len(mock.uploads), len(mock.items))
	}
}

func TestNegativeRegistrationAcknowledgementReconcilesAppliedItem(t *testing.T) {
	mock := newMockDegoo()
	mock.falseRegisterAcknowledgement = true
	mock.applyFalseRegisterAck = true
	mock.registrationVisibilityLag = 250 * time.Millisecond
	f, server := newTestFs(t, mock)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	content := []byte("generated encrypted payload")
	obj, err := f.Put(ctx, bytes.NewReader(content), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(content)), true, nil, f))
	if err != nil {
		t.Fatalf("Put after false acknowledgement with an applied registration: %v", err)
	}
	if obj == nil {
		t.Fatal("Put returned a nil object after reconciling the applied registration")
	}
	body, err := obj.Open(ctx)
	if err != nil {
		t.Fatalf("Open reconciled object: %v", err)
	}
	got, err := io.ReadAll(body)
	closeTestBody(t, body)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("verified object content = %q, want %q (read error: %v)", got, content, err)
	}
	visible, err := f.NewObject(ctx, "cipher")
	if err != nil {
		t.Fatalf("published object lookup: %v", err)
	}
	if visible == nil || visible.Remote() != "cipher" {
		t.Fatalf("published object = %v, want cipher", visible)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.registerCalls != 1 || len(mock.uploads) != 1 {
		t.Fatalf("reconciled registration replayed: %d registrations, %d uploads; want 1, 1", mock.registerCalls, len(mock.uploads))
	}
	if len(mock.items) != 2 {
		t.Fatalf("metadata item count = %d, want vault and one published item", len(mock.items))
	}
	for _, item := range mock.items {
		if item.name == "cipher" && (item.parentID != "vault" || item.size != int64(len(content))) {
			t.Fatalf("published metadata = parent %q, size %d; want parent vault and size %d", item.parentID, item.size, len(content))
		}
	}
}

func TestWaitForItemFailsFastOnProviderCooldown(t *testing.T) {
	mock := newMockDegoo()
	mock.listRetryAfter = "120"
	f, server := newTestFs(t, mock)
	defer server.Close()
	mock.mu.Lock()
	initialListCalls := mock.listCalls
	mock.mu.Unlock()
	started := time.Now()
	_, err := f.waitForItem(context.Background(), "vault", "missing", 10*time.Second)
	if err == nil {
		t.Fatal("lookup succeeded for a missing item")
	}
	var rateLimitErr *api.RateLimitError
	if !errors.As(err, &rateLimitErr) || api.Retryable(err) {
		t.Fatalf("lookup error = %v; want non-retryable provider cooldown", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("lookup waited during provider cooldown: %v", elapsed)
	}
	secondStarted := time.Now()
	if _, err := f.listDir(context.Background(), "vault"); err == nil || !errors.As(err, &rateLimitErr) {
		t.Fatalf("lookup during persisted cooldown = %v, want rate-limit error", err)
	}
	if elapsed := time.Since(secondStarted); elapsed > 100*time.Millisecond {
		t.Fatalf("lookup did not fail fast during persisted cooldown: %v", elapsed)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.listCalls != initialListCalls+1 {
		t.Fatalf("listing requests = %d, want %d (one failed request; cooldown blocks retries)", mock.listCalls, initialListCalls+1)
	}
}

func TestMetadataCallBoundsBlockedRequests(t *testing.T) {
	tests := []struct {
		name  string
		block func(*mockDegoo)
		call  func(*Fs, context.Context) error
	}{
		{
			name:  "list",
			block: func(mock *mockDegoo) { mock.blockListing = true },
			call: func(f *Fs, ctx context.Context) error {
				_, err := f.listDir(ctx, "vault")
				return err
			},
		},
		{
			name:  "get item",
			block: func(mock *mockDegoo) { mock.blockGetItem = true },
			call: func(f *Fs, ctx context.Context) error {
				_, err := f.getItem(ctx, "vault")
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock := newMockDegoo()
			f, server := newTestFs(t, mock)
			defer server.Close()
			mock.mu.Lock()
			initialListCalls, initialGetItemCalls := mock.listCalls, mock.getItemCalls
			test.block(mock)
			mock.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			err := test.call(f, ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("metadata lookup error = %v, want context deadline", err)
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("metadata lookup exceeded caller deadline: %v", elapsed)
			}
			mock.mu.Lock()
			defer mock.mu.Unlock()
			if test.name == "list" && mock.listCalls != initialListCalls+1 {
				t.Fatalf("listing requests = %d, want %d", mock.listCalls, initialListCalls+1)
			}
			if test.name == "get item" && mock.getItemCalls != initialGetItemCalls+1 {
				t.Fatalf("GetItem requests = %d, want %d", mock.getItemCalls, initialGetItemCalls+1)
			}
		})
	}
}

func TestMetadataCallDeadlineCoversAllListingPages(t *testing.T) {
	mock := newMockDegoo()
	mock.listNextToken = "second-page"
	mock.blockListingPage2 = true
	mock.listPage2Started = make(chan struct{}, 1)
	f, server := newTestFs(t, mock)
	defer server.Close()

	started := time.Now()
	err := metadataCallWithTimeout(context.Background(), 150*time.Millisecond, func(ctx context.Context) error {
		_, err := f.api.ListDir(ctx, "vault")
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("metadata listing error = %v, want the shared lookup deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("paginated metadata lookup exceeded its total deadline: %v", elapsed)
	}
	select {
	case <-mock.listPage2Started:
	default:
		t.Fatal("listing did not reach page two before the shared deadline")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.listCalls != 2 {
		t.Fatalf("listing requests = %d, want two pages under one deadline", mock.listCalls)
	}
}

func TestMetadataCallRetriesRetryableErrorsWithinBound(t *testing.T) {
	mock := newMockDegoo()
	mock.listStatusCode = http.StatusInternalServerError
	f, server := newTestFs(t, mock)
	defer server.Close()
	mock.mu.Lock()
	initialListCalls := mock.listCalls
	mock.mu.Unlock()

	started := time.Now()
	_, err := f.listDir(context.Background(), "vault")
	if api.HTTPStatus(err) != http.StatusInternalServerError {
		t.Fatalf("listing error status = %d, want %d (error: %v)", api.HTTPStatus(err), http.StatusInternalServerError, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("metadata retry exceeded its bounded delays: %v", elapsed)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if got := mock.listCalls - initialListCalls; got != metadataMaxAttempts {
		t.Fatalf("metadata listing requests = %d, want %d", got, metadataMaxAttempts)
	}
}

func TestNewFsUsesRootValidationProofDuringCooldown(t *testing.T) {
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()

	proofData, err := auth.ReadPrivateFile(f.rootValidationProofPath())
	if err != nil {
		t.Fatalf("read saved root proof: %v", err)
	}
	var proof rootValidationProof
	if err := json.Unmarshal(proofData, &proof); err != nil || proof.Version != rootProofVersion || !proof.Validated || proof.APIURL != f.opt.APIURL || proof.RootFolderID != f.opt.RootFolderID {
		t.Fatalf("saved root proof = %#v, decode error %v", proof, err)
	}

	until := time.Now().Add(2*time.Minute).Unix() + 1
	rateLimitData, err := json.Marshal(struct {
		Version   int   `json:"version"`
		UntilUnix int64 `json:"until_unix"`
	}{Version: 1, UntilUnix: until})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.WritePrivateFile(f.opt.AuthFile+".rate-limit.json", rateLimitData); err != nil {
		t.Fatalf("write synthetic rate-limit state: %v", err)
	}

	mock.mu.Lock()
	initialGetItemCalls := mock.getItemCalls
	mock.mu.Unlock()
	options := configmap.Simple{
		"auth_file": f.opt.AuthFile, "root_folder_id": f.opt.RootFolderID, "api_key": f.opt.APIKey,
		"api_url": f.opt.APIURL, "rest_url": f.opt.RESTURL,
	}
	offline, err := NewFs(context.Background(), "test", "", options)
	if err != nil {
		t.Fatalf("NewFs during a matching provider cooldown: %v", err)
	}
	if offline.Root() != "" {
		t.Fatalf("offline root = %q, want vault root", offline.Root())
	}
	mock.mu.Lock()
	if mock.getItemCalls != initialGetItemCalls {
		t.Fatalf("GetItem requests during cooldown = %d, want %d", mock.getItemCalls, initialGetItemCalls)
	}
	mock.mu.Unlock()

	options["root_folder_id"] = "different-root"
	if _, err := NewFs(context.Background(), "test", "", options); err == nil {
		t.Fatal("NewFs accepted a root proof for a different root ID")
	}
	options["root_folder_id"] = f.opt.RootFolderID
	if _, err := NewFs(context.Background(), "test", "nested", options); err == nil {
		t.Fatal("NewFs accepted offline root validation for a non-empty backend path")
	}
}

func TestOpenReusesFreshListedDownloadURL(t *testing.T) {
	mock := newMockDegoo()
	content := []byte("generated encrypted test payload")
	mock.items["payload"] = mockItem{id: "payload", name: "cipher", parentID: "vault", category: 6, size: int64(len(content)), content: content}
	f, server := newTestFs(t, mock)
	defer server.Close()
	obj, err := f.NewObject(context.Background(), "cipher")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	mock.mu.Lock()
	getItemCalls, downloadCalls := mock.getItemCalls, mock.downloadCalls
	mock.mu.Unlock()
	body, err := obj.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(body)
	closeTestBody(t, body)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("downloaded payload = %q, read error %v", got, err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.getItemCalls != getItemCalls {
		t.Fatalf("GetItem requests for fresh listed URL = %d, want %d", mock.getItemCalls-getItemCalls, 0)
	}
	if mock.downloadCalls != downloadCalls+1 {
		t.Fatalf("download requests = %d, want %d", mock.downloadCalls-downloadCalls, 1)
	}
}

func TestOpenRefreshesExpiredOrRejectedListedURL(t *testing.T) {
	for _, test := range []struct {
		name        string
		expireByAge bool
	}{
		{name: "proactive age", expireByAge: true},
		{name: "provider rejection"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mock := newMockDegoo()
			content := []byte("generated encrypted test payload")
			mock.items["payload"] = mockItem{id: "payload", name: "cipher", parentID: "vault", category: 6, size: int64(len(content)), content: content}
			f, server := newTestFs(t, mock)
			defer server.Close()
			obj, err := f.NewObject(context.Background(), "cipher")
			if err != nil {
				t.Fatalf("NewObject: %v", err)
			}
			if test.expireByAge {
				obj.(*Object).urlAt = time.Now().Add(-downloadURLMaxAge - time.Second)
			} else {
				mock.mu.Lock()
				mock.expireNextDownload = true
				mock.mu.Unlock()
			}
			mock.mu.Lock()
			getItemCalls, downloadCalls := mock.getItemCalls, mock.downloadCalls
			mock.mu.Unlock()

			body, err := obj.Open(context.Background())
			if err != nil {
				t.Fatalf("Open after URL expiry: %v", err)
			}
			got, err := io.ReadAll(body)
			closeTestBody(t, body)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("downloaded payload = %q, read error %v", got, err)
			}
			mock.mu.Lock()
			defer mock.mu.Unlock()
			if mock.getItemCalls != getItemCalls+1 {
				t.Fatalf("GetItem requests = %d, want 1", mock.getItemCalls-getItemCalls)
			}
			wantDownloads := 1
			if !test.expireByAge {
				wantDownloads = 2
			}
			if got := mock.downloadCalls - downloadCalls; got != wantDownloads {
				t.Fatalf("download requests = %d, want %d", got, wantDownloads)
			}
		})
	}
}

func TestUploadPublicationRechecksTargetBeforePublish(t *testing.T) {
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()
	mock.mu.Lock()
	initialListCalls, initialGetItemCalls := mock.listCalls, mock.getItemCalls
	mock.mu.Unlock()

	content := []byte("generated encrypted test payload")
	_, err := f.Put(context.Background(), bytes.NewReader(content), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(content)), true, nil, f))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if got := mock.listCalls - initialListCalls; got != 3 {
		t.Fatalf("parent listing requests = %d, want cached Put lookup, fresh registration visibility, and fresh pre-publication target check", got)
	}
	if got := mock.getItemCalls - initialGetItemCalls; got != 2 {
		t.Fatalf("targeted metadata requests = %d, want upload verification and publication confirmation", got)
	}
}

func TestWaitForItemBacksOffRetryableListingErrors(t *testing.T) {
	mock := newMockDegoo()
	mock.listStatusCode = http.StatusInternalServerError
	f, server := newTestFs(t, mock)
	defer server.Close()
	mock.mu.Lock()
	initialListCalls := mock.listCalls
	mock.mu.Unlock()
	_, err := f.waitForItem(context.Background(), "vault", "missing", 1500*time.Millisecond)
	if api.HTTPStatus(err) != http.StatusInternalServerError {
		t.Fatalf("lookup error status = %d, want %d (error: %v)", api.HTTPStatus(err), http.StatusInternalServerError, err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if got := mock.listCalls - initialListCalls; got != 3 {
		t.Fatalf("retryable listing requests = %d, want 3 with increasing backoff", got)
	}
}

func TestWaitForIDsGoneBacksOffWhileItemRemainsVisible(t *testing.T) {
	mock := newMockDegoo()
	mock.items["target"] = mockItem{id: "target", name: "cipher", parentID: "vault", category: 6}
	f, server := newTestFs(t, mock)
	defer server.Close()
	mock.mu.Lock()
	initialListCalls := mock.listCalls
	mock.mu.Unlock()

	err := f.waitForIDsGone(context.Background(), "vault", []string{"target"}, 1500*time.Millisecond)
	if err == nil {
		t.Fatal("waitForIDsGone succeeded while the item remained visible")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if got := mock.listCalls - initialListCalls; got != 3 {
		t.Fatalf("confirmation listing requests = %d, want 3 with 250/500/1000ms backoff", got)
	}
}

func TestWaitForPublishedObjectBacksOffUntilMetadataMatches(t *testing.T) {
	mock := newMockDegoo()
	mock.items["staged"] = mockItem{id: "staged", name: temporaryPrefix + "pending", parentID: "vault", category: 6, size: 4}
	f, server := newTestFs(t, mock)
	defer server.Close()
	mock.mu.Lock()
	initialGetItemCalls := mock.getItemCalls
	mock.mu.Unlock()

	staged := api.Item{ID: "staged", Name: temporaryPrefix + "pending", ParentID: "vault", Size: 4}
	_, err := f.waitForPublishedObject(context.Background(), "cipher", "cipher", "vault", staged, "vault", 4, 1500*time.Millisecond)
	if err == nil {
		t.Fatal("waitForPublishedObject succeeded while the pending name remained visible")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if got := mock.getItemCalls - initialGetItemCalls; got != 3 {
		t.Fatalf("published metadata requests = %d, want 3 with 250/500/1000ms backoff", got)
	}
}

func TestRegistrationLookupTimeoutCancelsBlockedRequest(t *testing.T) {
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()
	mock.mu.Lock()
	mock.blockListing = true
	mock.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	_, err := f.waitForItem(ctx, "vault", "missing", 50*time.Millisecond)
	if err == nil {
		t.Fatal("lookup succeeded for a missing item")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("lookup exceeded its timeout: %v", elapsed)
	}
}

func TestRegistrationFailureLeavesPriorObjectReachable(t *testing.T) {
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()
	ctx := context.Background()
	original := []byte("known good old encrypted content")
	obj, err := f.Put(ctx, strings.NewReader(string(original)), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(original)), true, nil, f))
	if err != nil {
		t.Fatalf("initial Put: %v", err)
	}

	mock.mu.Lock()
	mock.failRegister = true
	mock.registerSeen = make(chan struct{}, 1)
	seen := mock.registerSeen
	mock.mu.Unlock()
	updateCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	replacement := []byte("replacement that must not publish")
	go func() {
		done <- obj.(*Object).Update(updateCtx, strings.NewReader(string(replacement)), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(replacement)), true, nil, f))
	}()
	select {
	case <-seen:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("registration request was not observed")
	}
	if err := <-done; err == nil {
		t.Fatal("Update succeeded after registration failure")
	}
	current, err := f.NewObject(ctx, "cipher")
	if err != nil {
		t.Fatalf("old path was lost after failed update: %v", err)
	}
	body, err := current.Open(ctx)
	if err != nil {
		t.Fatalf("Open old object: %v", err)
	}
	got, err := io.ReadAll(body)
	closeTestBody(t, body)
	if err != nil {
		t.Fatalf("read old object: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("old content changed to %q", got)
	}
	if current.(*Object).ID() != obj.(*Object).ID() {
		t.Fatalf("current ID = %q, want old ID %q", current.(*Object).ID(), obj.(*Object).ID())
	}
}

func TestUploadVerificationFailureLeavesPriorObjectReachable(t *testing.T) {
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()
	ctx := context.Background()
	original := []byte("verified old ciphertext")
	obj, err := f.Put(ctx, strings.NewReader(string(original)), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(original)), true, nil, f))
	if err != nil {
		t.Fatalf("initial Put: %v", err)
	}
	mock.mu.Lock()
	mock.corruptUpload = true
	mock.mu.Unlock()
	replacement := []byte("transferred ciphertext was corrupted")
	err = obj.(*Object).Update(ctx, strings.NewReader(string(replacement)), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(replacement)), true, nil, f))
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("Update error = %v, want upload verification failure", err)
	}
	current, err := f.NewObject(ctx, "cipher")
	if err != nil {
		t.Fatalf("old path was lost after verification failure: %v", err)
	}
	if current.(*Object).ID() != obj.(*Object).ID() {
		t.Fatalf("current ID = %q, want old ID %q", current.(*Object).ID(), obj.(*Object).ID())
	}
	body, err := current.Open(ctx)
	if err != nil {
		t.Fatalf("Open old object: %v", err)
	}
	got, err := io.ReadAll(body)
	closeTestBody(t, body)
	if err != nil || string(got) != string(original) {
		t.Fatalf("old content = %q, %v; want %q", got, err, original)
	}
}

func TestSkipUploadVerificationSkipsPayloadReadbackButChecksMetadata(t *testing.T) {
	t.Run("payload readback skipped", func(t *testing.T) {
		mock := newMockDegoo()
		mock.corruptUpload = true
		f, server, err := newTestFsAtRootID(t, mock, "", "vault", configmap.Simple{"skip_upload_verification": "true"})
		if err != nil {
			server.Close()
			t.Fatalf("NewFs: %v", err)
		}
		defer server.Close()
		data := []byte("generated encrypted payload")
		obj, err := f.Put(context.Background(), strings.NewReader(string(data)), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(data)), true, nil, f))
		if err != nil {
			t.Fatalf("Put with skip_upload_verification: %v", err)
		}
		published, err := f.NewObject(context.Background(), "cipher")
		if err != nil {
			t.Fatalf("NewObject after publication: %v", err)
		}
		if published.(*Object).ID() != obj.(*Object).ID() || published.(*Object).ParentID() != "vault" || published.Size() != int64(len(data)) {
			t.Fatalf("published metadata = (id %q, parent %q, size %d), want registered item metadata", published.(*Object).ID(), published.(*Object).ParentID(), published.Size())
		}
		mock.mu.Lock()
		downloadCalls := mock.downloadCalls
		mock.mu.Unlock()
		if downloadCalls != 0 {
			t.Fatalf("download calls = %d, want no payload readback", downloadCalls)
		}
	})

	t.Run("registered metadata still required", func(t *testing.T) {
		mock := newMockDegoo()
		mock.registeredSizeDelta = 1
		f, server, err := newTestFsAtRootID(t, mock, "", "vault", configmap.Simple{"skip_upload_verification": "true"})
		if err != nil {
			server.Close()
			t.Fatalf("NewFs: %v", err)
		}
		defer server.Close()
		data := []byte("generated encrypted payload")
		_, err = f.Put(context.Background(), strings.NewReader(string(data)), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(data)), true, nil, f))
		if err == nil || !strings.Contains(err.Error(), "registered upload size mismatch") {
			t.Fatalf("Put error = %v, want registered metadata size mismatch", err)
		}
		if _, err := f.NewObject(context.Background(), "cipher"); !errors.Is(err, fs.ErrorObjectNotFound) {
			t.Fatalf("NewObject after bad metadata = %v, want object not found", err)
		}
		mock.mu.Lock()
		downloadCalls := mock.downloadCalls
		mock.mu.Unlock()
		if downloadCalls != 0 {
			t.Fatalf("download calls = %d, want no payload readback", downloadCalls)
		}
	})
}

func TestEmptyUploadAndMarkerDirectory(t *testing.T) {
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()
	ctx := context.Background()
	if err := f.Mkdir(ctx, "folder"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Remote() != "folder" {
		t.Fatalf("visible entries = %#v, want only folder", entries)
	}
	if _, ok := entries[0].(fs.Directory); !ok {
		t.Fatalf("folder entry type = %T, want fs.Directory", entries[0])
	}
	obj, err := f.Put(ctx, strings.NewReader(""), object.NewStaticObjectInfo("empty", time.Now(), 0, true, nil, f))
	if err != nil {
		t.Fatalf("empty Put: %v", err)
	}
	body, err := obj.Open(ctx)
	if err != nil {
		t.Fatalf("Open empty object: %v", err)
	}
	data, err := io.ReadAll(body)
	closeTestBody(t, body)
	if err != nil || len(data) != 0 {
		t.Fatalf("empty object read = %q, %v", data, err)
	}
}

func TestDirectoryPlaceholderAndDuplicateNames(t *testing.T) {
	mock := newMockDegoo()
	mock.items["real-folder"] = mockItem{id: "real-folder", name: "folder", parentID: "vault", category: 2}
	mock.items["placeholder"] = mockItem{id: "placeholder", name: "folder", parentID: "vault", category: 6, size: 0}
	mock.items["marker"] = mockItem{id: "marker", name: directoryMarkerName("placeholder"), parentID: "real-folder", category: 6, size: 0}
	mock.items["empty-file"] = mockItem{id: "empty-file", name: "empty", parentID: "vault", category: 6, size: 0}
	mock.items["duplicate-1"] = mockItem{id: "duplicate-1", name: "ambiguous", parentID: "vault", category: 6, size: 4}
	mock.items["duplicate-2"] = mockItem{id: "duplicate-2", name: "ambiguous", parentID: "vault", category: 6, size: 4}
	f, server := newTestFs(t, mock)
	defer server.Close()
	_, err := f.List(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "ambiguous duplicate name") {
		t.Fatalf("List error = %v, want deterministic duplicate-name error", err)
	}

	mock.mu.Lock()
	delete(mock.items, "duplicate-1")
	delete(mock.items, "duplicate-2")
	mock.mu.Unlock()
	f.api.ClearMetadataCache()
	entries, err := f.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List after removing ambiguous entries: %v", err)
	}
	var folder fs.Directory
	var empty fs.Object
	for _, entry := range entries {
		switch entry.Remote() {
		case "folder":
			folder, _ = entry.(fs.Directory)
		case "empty":
			empty, _ = entry.(fs.Object)
		}
	}
	if folder == nil || folder.ID() != "real-folder" {
		t.Fatalf("folder entry = %#v, want promoted Cat2 ID real-folder", folder)
	}
	if empty == nil {
		t.Fatal("ordinary empty Cat6 item was not listed as a file")
	}
}

func TestCreateDirReconcilesCategoryPromotion(t *testing.T) {
	mock := newMockDegoo()
	mock.promoteOnMarker = true
	mock.items["untouched-empty"] = mockItem{id: "untouched-empty", name: "empty", parentID: "vault", category: 6, size: 0}
	f, server := newTestFs(t, mock)
	defer server.Close()
	ctx := context.Background()
	if err := f.Mkdir(ctx, "folder"); err != nil {
		t.Fatalf("Mkdir promoted folder: %v", err)
	}

	mock.mu.Lock()
	var promoted mockItem
	var placeholder mockItem
	var marker mockItem
	for _, item := range mock.items {
		switch {
		case item.name == "folder" && item.category == 2:
			promoted = item
		case strings.HasPrefix(item.name, retiredDirectoryPrefix) && item.category == 0:
			placeholder = item
		case strings.HasPrefix(item.name, directoryMarkerPrefix):
			marker = item
		}
	}
	mock.mu.Unlock()
	if promoted.id == "" || placeholder.id == "" || marker.id == "" {
		t.Fatalf("promotion state = promoted %#v, placeholder %#v, marker %#v", promoted, placeholder, marker)
	}
	if marker.parentID != promoted.id || marker.name != directoryMarkerName(placeholder.id) {
		t.Fatalf("marker = %#v, want marker hashed from placeholder %q under promoted folder %q", marker, placeholder.id, promoted.id)
	}
	resolvedID, err := f.findDir(ctx, "folder", false)
	if err != nil || resolvedID != promoted.id {
		t.Fatalf("resolved folder ID = %q, %v; want promoted ID %q", resolvedID, err, promoted.id)
	}
	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("root entries = %#v, want promoted folder and unrelated empty file", entries)
	}
	children, err := f.List(ctx, "folder")
	if err != nil || len(children) != 0 {
		t.Fatalf("List promoted folder = %#v, %v; want hidden marker only", children, err)
	}
	if err := f.Mkdir(ctx, "folder"); err != nil {
		t.Fatalf("idempotent Mkdir after promotion: %v", err)
	}
	if err := f.Rmdir(ctx, "folder"); err != nil {
		t.Fatalf("Rmdir promoted folder: %v", err)
	}
	mock.mu.Lock()
	_, placeholderExists := mock.items[placeholder.id]
	_, promotedExists := mock.items[promoted.id]
	_, markerExists := mock.items[marker.id]
	_, emptyExists := mock.items["untouched-empty"]
	mock.mu.Unlock()
	if placeholderExists || promotedExists || markerExists || !emptyExists {
		t.Fatalf("post-Rmdir items: placeholder=%v promoted=%v marker=%v unrelated-empty=%v", placeholderExists, promotedExists, markerExists, emptyExists)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, call := range mock.deleteCalls {
		if call.permanent {
			t.Fatalf("Rmdir used permanent deletion for internal item %q", call.id)
		}
	}
}

func TestCreateDirUsesMarkerToPromoteNewCategoryZeroPlaceholder(t *testing.T) {
	mock := newMockDegoo()
	mock.promoteOnMarker = true
	mock.initialPlaceholderCategory = 0
	f, server := newTestFs(t, mock)
	defer server.Close()

	if err := f.Mkdir(context.Background(), "folder"); err != nil {
		t.Fatalf("Mkdir category-zero placeholder: %v", err)
	}
	entries, err := f.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List after Mkdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Remote() != "folder" {
		t.Fatalf("root entries = %#v, want the created folder", entries)
	}
	if _, ok := entries[0].(*fs.Dir); !ok {
		t.Fatalf("root entry type = %T, want directory", entries[0])
	}
}

func TestCreateDirDoesNotAdoptExistingCategoryZeroEmptyFile(t *testing.T) {
	mock := newMockDegoo()
	mock.items["empty-file"] = mockItem{id: "empty-file", name: "empty", parentID: "vault", category: 0, size: 0}
	f, server := newTestFs(t, mock)
	defer server.Close()

	if _, err := f.CreateDir(context.Background(), "vault", "empty"); !errors.Is(err, fs.ErrorDirExists) {
		t.Fatalf("CreateDir existing category-zero empty file error = %v, want %v", err, fs.ErrorDirExists)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.items) != 2 {
		t.Fatalf("items after CreateDir = %#v, want no marker or new directory", mock.items)
	}
}

func TestCreateDirDoesNotAdoptUnconfirmedCategoryZeroPlaceholder(t *testing.T) {
	mock := newMockDegoo()
	mock.initialPlaceholderCategory = 0
	mock.failInitialMkdirAck = true
	f, server := newTestFs(t, mock)
	defer server.Close()

	if _, err := f.CreateDir(context.Background(), "vault", "folder"); err == nil || !strings.Contains(err.Error(), "unconfirmed create") {
		t.Fatalf("CreateDir error = %v, want unconfirmed category-zero refusal", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.items) != 2 {
		t.Fatalf("items after ambiguous create = %#v, want only the new unmarked placeholder", mock.items)
	}
	for _, item := range mock.items {
		if strings.HasPrefix(item.name, directoryMarkerPrefix) {
			t.Fatalf("ambiguous create was adopted and marker %q was written", item.name)
		}
	}
}

func TestCategoryPromotionDoesNotHideUnmarkedDuplicate(t *testing.T) {
	mock := newMockDegoo()
	mock.items["promoted"] = mockItem{id: "promoted", name: "folder", parentID: "vault", category: 2}
	mock.items["placeholder"] = mockItem{id: "placeholder", name: "folder", parentID: "vault", category: 0, size: 0}
	mock.items["empty-file"] = mockItem{id: "empty-file", name: "folder", parentID: "vault", category: 6, size: 0}
	mock.items["marker"] = mockItem{id: "marker", name: directoryMarkerName("placeholder"), parentID: "promoted", category: 6, size: 0}
	f, server := newTestFs(t, mock)
	defer server.Close()
	if _, err := f.List(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "ambiguous duplicate name") {
		t.Fatalf("List error = %v, want ambiguity for unmarked duplicate", err)
	}
}

func TestRetiredPlaceholderPreventsSameNameRenameRouting(t *testing.T) {
	mock := newMockDegoo()
	mock.renameRoutesViaAlias = true
	mock.items["ghost-folder"] = mockItem{id: "ghost-folder", name: "folder", parentID: "vault", category: 0, size: 0}
	mock.items["real-folder"] = mockItem{id: "real-folder", name: "folder", parentID: "vault", category: 2}
	mock.items["folder-marker"] = mockItem{id: "folder-marker", name: directoryMarkerName("ghost-folder"), parentID: "real-folder", category: 6, size: 0}
	f, server := newTestFs(t, mock)
	defer server.Close()
	ctx := context.Background()

	mock.mu.Lock()
	mock.items["routing-probe"] = mockItem{id: "routing-probe", name: temporaryPrefix + "probe", parentID: "real-folder", category: 6, size: 1}
	mock.mu.Unlock()
	if err := f.api.Rename(ctx, "routing-probe", "probe-final"); err != nil {
		t.Fatalf("rename preflight probe: %v", err)
	}
	mock.mu.Lock()
	preflight := mock.items["routing-probe"]
	delete(mock.items, "routing-probe")
	mock.mu.Unlock()
	if preflight.parentID != "ghost-folder" {
		t.Fatalf("mock alias routing parent = %q, want ghost alias before retirement", preflight.parentID)
	}

	folderID, found, err := f.FindLeaf(ctx, "vault", "folder")
	if err != nil || !found || folderID != "real-folder" {
		t.Fatalf("FindLeaf folder = %q, %v, %v; want promoted directory", folderID, found, err)
	}
	mock.mu.Lock()
	ghost := mock.items["ghost-folder"]
	mock.mu.Unlock()
	if ghost.name != retiredDirectoryName(ghost.id) || !isInternalName(ghost.name) {
		t.Fatalf("ghost name = %q, want hidden retired alias", ghost.name)
	}

	content := []byte("encrypted test data")
	obj, err := f.Put(ctx, strings.NewReader(string(content)), object.NewStaticObjectInfo("folder/cipher", time.Now(), int64(len(content)), true, nil, f))
	if err != nil {
		t.Fatalf("Put through promoted folder: %v", err)
	}
	if obj.(*Object).ParentID() != "real-folder" {
		t.Fatalf("published parent = %q, want promoted directory", obj.(*Object).ParentID())
	}
	if err := obj.Remove(ctx); err != nil {
		t.Fatalf("remove test object: %v", err)
	}
	if err := f.Rmdir(ctx, "folder"); err != nil {
		t.Fatalf("Rmdir folder with retired alias: %v", err)
	}
	mock.mu.Lock()
	_, ghostExists := mock.items["ghost-folder"]
	_, folderExists := mock.items["real-folder"]
	_, markerExists := mock.items["folder-marker"]
	mock.mu.Unlock()
	if ghostExists || folderExists || markerExists {
		t.Fatalf("Rmdir left internal items: ghost=%v folder=%v marker=%v", ghostExists, folderExists, markerExists)
	}
}

func TestNonemptyPromotedPlaceholderIsNotRetired(t *testing.T) {
	mock := newMockDegoo()
	mock.items["ghost-folder"] = mockItem{id: "ghost-folder", name: "folder", parentID: "vault", category: 0, size: 0}
	mock.items["real-folder"] = mockItem{id: "real-folder", name: "folder", parentID: "vault", category: 2}
	mock.items["folder-marker"] = mockItem{id: "folder-marker", name: directoryMarkerName("ghost-folder"), parentID: "real-folder", category: 6, size: 0}
	mock.items["preserved-user-file"] = mockItem{id: "preserved-user-file", name: "user-data", parentID: "ghost-folder", category: 6, size: 5}
	f, server := newTestFs(t, mock)
	defer server.Close()

	if _, _, err := f.FindLeaf(context.Background(), "vault", "folder"); err == nil || !strings.Contains(err.Error(), "contains data") {
		t.Fatalf("FindLeaf error = %v, want fail-closed nonempty alias error", err)
	}
	mock.mu.Lock()
	ghost := mock.items["ghost-folder"]
	_, childExists := mock.items["preserved-user-file"]
	mock.mu.Unlock()
	if ghost.name != "folder" || !childExists {
		t.Fatalf("nonempty alias changed: name=%q child-preserved=%v", ghost.name, childExists)
	}
}

func TestNewFsRetiresConfiguredRootPlaceholder(t *testing.T) {
	mock := newMockDegoo()
	mock.items["device"] = mockItem{id: "device", name: "device", category: 1}
	mock.items["root-placeholder"] = mockItem{id: "root-placeholder", name: "vault-root", parentID: "device", category: 0, size: 0}
	mock.items["real-root"] = mockItem{id: "real-root", name: "vault-root", parentID: "device", category: 2}
	mock.items["root-marker"] = mockItem{id: "root-marker", name: directoryMarkerName("root-placeholder"), parentID: "real-root", category: 6, size: 0}
	f, server, err := newTestFsAtRootID(t, mock, "", "real-root")
	defer server.Close()
	if err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	if got := f.(*Fs).RootFolderID(); got != "real-root" {
		t.Fatalf("RootFolderID = %q, want configured root", got)
	}
	mock.mu.Lock()
	ghost := mock.items["root-placeholder"]
	mock.mu.Unlock()
	if ghost.name != retiredDirectoryName(ghost.id) {
		t.Fatalf("configured root placeholder name = %q, want retired internal name", ghost.name)
	}
}

func TestReplacementWaitsForPublishedListing(t *testing.T) {
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()
	ctx := context.Background()
	oldData := []byte("previous encrypted version")
	old, err := f.Put(ctx, strings.NewReader(string(oldData)), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(oldData)), true, nil, f))
	if err != nil {
		t.Fatalf("initial Put: %v", err)
	}
	oldID := old.(*Object).ID()
	mock.mu.Lock()
	mock.listLagForName = "cipher"
	mock.listLagResponses = 3
	mock.overlayParentZero = true
	mock.mu.Unlock()

	newData := []byte("replacement encrypted version")
	if err := old.Update(ctx, strings.NewReader(string(newData)), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(newData)), true, nil, f)); err != nil {
		t.Fatalf("Update with delayed listing: %v", err)
	}
	current, err := f.NewObject(ctx, "cipher")
	if err != nil {
		t.Fatalf("NewObject after delayed listing: %v", err)
	}
	if current.(*Object).ID() == oldID {
		t.Fatalf("current ID = %q, want replacement distinct from old ID %q", current.(*Object).ID(), oldID)
	}
	if current.(*Object).ParentID() != "vault" {
		t.Fatalf("published parent ID = %q, want canonical listing parent %q", current.(*Object).ParentID(), "vault")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if !mock.getItemSawFinalName {
		t.Fatal("GetItem never confirmed the final name during listing lag")
	}
	if mock.backupDeletedBeforeVisible {
		t.Fatal("old backup was deleted before the published listing became visible")
	}
	if _, exists := mock.items[oldID]; exists {
		t.Fatal("old backup remains after published replacement was confirmed")
	}
	if len(mock.deleteCalls) != 1 || mock.deleteCalls[0].id != oldID || mock.deleteCalls[0].permanent {
		t.Fatalf("replacement delete calls = %#v, want one recycle-bin deletion of old object", mock.deleteCalls)
	}
}

func TestRemoveRecyclesBackupsBeforeActiveObject(t *testing.T) {
	mock := newMockDegoo()
	backupName, err := newBackupName("cipher")
	if err != nil {
		t.Fatal(err)
	}
	mock.items["active"] = mockItem{id: "active", name: "cipher", parentID: "vault", category: 6, size: 4, content: []byte("live")}
	mock.items["backup"] = mockItem{id: "backup", name: backupName, parentID: "vault", category: 6, size: 3, content: []byte("old")}
	f, server := newTestFs(t, mock)
	defer server.Close()

	obj, err := f.NewObject(context.Background(), "cipher")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if err := obj.Remove(context.Background()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.deleteCalls) != 2 || mock.deleteCalls[0].id != "backup" || mock.deleteCalls[1].id != "active" {
		t.Fatalf("delete order = %#v, want backup before active object", mock.deleteCalls)
	}
	for _, call := range mock.deleteCalls {
		if call.permanent {
			t.Fatalf("Remove used permanent deletion for %q", call.id)
		}
	}
	if len(mock.items) != 1 {
		t.Fatalf("remaining items = %#v, want only vault", mock.items)
	}
}

func TestRemoveRetainsActiveObjectWhenBackupCleanupFails(t *testing.T) {
	mock := newMockDegoo()
	backupName, err := newBackupName("cipher")
	if err != nil {
		t.Fatal(err)
	}
	mock.items["active"] = mockItem{id: "active", name: "cipher", parentID: "vault", category: 6, size: 4, content: []byte("live")}
	mock.items["backup"] = mockItem{id: "backup", name: backupName, parentID: "vault", category: 6, size: 3, content: []byte("old")}
	mock.failDeleteIDs = map[string]bool{"backup": true}
	f, server := newTestFs(t, mock)
	defer server.Close()

	obj, err := f.NewObject(context.Background(), "cipher")
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if err := obj.Remove(context.Background()); err == nil {
		t.Fatal("Remove succeeded despite backup cleanup failure")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if _, exists := mock.items["active"]; !exists {
		t.Fatal("active object was deleted despite backup cleanup failure")
	}
	if _, exists := mock.items["backup"]; !exists {
		t.Fatal("backup unexpectedly disappeared after simulated delete failure")
	}
	if len(mock.deleteCalls) != 1 || mock.deleteCalls[0].id != "backup" || mock.deleteCalls[0].permanent {
		t.Fatalf("delete calls = %#v, want only a recycle-bin deletion attempt for backup", mock.deleteCalls)
	}
}

func TestWaitForIDsGoneRequiresAbsence(t *testing.T) {
	mock := newMockDegoo()
	mock.items["target"] = mockItem{id: "target", name: "file", parentID: "vault", category: 6}
	f, server := newTestFs(t, mock)
	defer server.Close()

	if err := f.waitForIDsGone(context.Background(), "vault", []string{"target"}, time.Millisecond); err == nil {
		t.Fatal("waitForIDsGone succeeded while the target remained visible")
	}
	mock.mu.Lock()
	delete(mock.items, "target")
	mock.mu.Unlock()
	f.api.ClearMetadataCache()
	if err := f.waitForIDsGone(context.Background(), "vault", []string{"target"}, time.Second); err != nil {
		t.Fatalf("waitForIDsGone for absent target: %v", err)
	}
}

func TestWaitForIDsGoneBypassesStaleListingSnapshot(t *testing.T) {
	mock := newMockDegoo()
	mock.items["target"] = mockItem{id: "target", name: "file", parentID: "vault", category: 6}
	f, server := newTestFs(t, mock)
	defer server.Close()

	if _, err := f.listDir(context.Background(), "vault"); err != nil {
		t.Fatalf("prime cached listing: %v", err)
	}
	mock.mu.Lock()
	delete(mock.items, "target")
	mock.mu.Unlock()
	// Keep the old snapshot to verify that reconciliation explicitly requests fresh metadata.
	if err := f.waitForIDsGone(context.Background(), "vault", []string{"target"}, 500*time.Millisecond); err != nil {
		t.Fatalf("waitForIDsGone with a stale cached target listing: %v", err)
	}
}

func TestPublishedParentDiagnosticComparesPreAndPostRename(t *testing.T) {
	mock := newMockDegoo()
	mock.items["staged-private-id"] = mockItem{
		id: "staged-private-id", name: "final-cipher-name", parentID: "vault", category: 6, size: 4,
		content: []byte("data"),
	}
	f, server := newTestFs(t, mock)
	defer server.Close()

	staged := api.Item{ID: "staged-private-id", Name: temporaryPrefix + "pending", ParentID: "vault", Size: 4}
	if _, err := f.listDir(context.Background(), "vault"); err != nil {
		t.Fatalf("prime canonical listing: %v", err)
	}
	mock.mu.Lock()
	item := mock.items["staged-private-id"]
	item.parentID = "other-private-parent"
	mock.items[item.id] = item
	mock.mu.Unlock()
	mock.mu.Lock()
	initialListCalls := mock.listCalls
	mock.mu.Unlock()
	_, err := f.waitForPublishedObject(context.Background(), "final-cipher-name", "final-cipher-name", "vault", staged, "vault", 4, time.Millisecond)
	if err == nil {
		t.Fatal("waitForPublishedObject succeeded with an unpublished parent")
	}
	mock.mu.Lock()
	if got := mock.listCalls - initialListCalls; got != 1 {
		mock.mu.Unlock()
		t.Fatalf("canonical parent listing requests after parent mismatch = %d, want 1", got)
	}
	mock.mu.Unlock()
	for _, want := range []string{
		"pre_parent_expected=true", "pre_parent_staged=true", "pre_parent_vault=true",
		"post_parent_expected=false", "post_parent_staged=false", "post_parent_vault=false",
		"post_parent_same_as_pre=false",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	for _, privateValue := range []string{"staged-private-id", "other-private-parent", "final-cipher-name"} {
		if strings.Contains(err.Error(), privateValue) {
			t.Errorf("error exposes private provider value %q: %v", privateValue, err)
		}
	}
}

func TestUnsafePathsAreRejectedBeforeMutation(t *testing.T) {
	f := &Fs{opt: Options{RootFolderID: "vault"}}
	obj := &Object{fs: f, remote: "safe", id: "item"}
	unsafe := "../outside"
	if err := f.Mkdir(context.Background(), unsafe); err == nil {
		t.Fatal("Mkdir accepted parent traversal")
	}
	if err := f.Rmdir(context.Background(), unsafe); err == nil {
		t.Fatal("Rmdir accepted parent traversal")
	}
	if _, err := f.Put(context.Background(), strings.NewReader(""), object.NewStaticObjectInfo(unsafe, time.Now(), 0, true, nil, nil)); err == nil {
		t.Fatal("Put accepted parent traversal")
	}
	if _, err := f.Move(context.Background(), obj, unsafe); err == nil {
		t.Fatal("Move accepted parent traversal")
	}
	if err := f.DirMove(context.Background(), f, "safe", unsafe); err == nil {
		t.Fatal("DirMove accepted parent traversal")
	}
	obj.remote = unsafe
	if err := obj.Update(context.Background(), strings.NewReader(""), object.NewStaticObjectInfo(unsafe, time.Now(), 0, true, nil, nil)); err == nil {
		t.Fatal("Update accepted parent traversal")
	}
	if err := obj.Remove(context.Background()); err == nil {
		t.Fatal("Remove accepted parent traversal")
	}
}

func TestBackupPathParsing(t *testing.T) {
	name, err := newBackupName("encrypted/leaf")
	if err == nil {
		t.Fatal("backup name accepted a path separator")
	}
	if _, ok := backupTarget(name); ok {
		t.Fatal("invalid backup name was parsed")
	}
	name, err = newBackupName("cipher-leaf")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := backupTarget(name)
	if !ok || got != "cipher-leaf" {
		t.Fatalf("backupTarget(%q) = %q, %v", name, got, ok)
	}
}

func TestInterruptedReplacementBackupRemainsReachable(t *testing.T) {
	mock := newMockDegoo()
	content := []byte("old object retained during an interrupted replacement")
	backupName, err := newBackupName("cipher")
	if err != nil {
		t.Fatal(err)
	}
	mock.items["backup"] = mockItem{id: "backup", name: backupName, parentID: "vault", category: 6, size: int64(len(content)), created: time.Now(), content: content}
	f, server := newTestFs(t, mock)
	defer server.Close()
	entries, err := f.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List recovered backup: %v", err)
	}
	if len(entries) != 1 || entries[0].Remote() != "cipher" {
		t.Fatalf("entries = %#v, want recovered cipher path", entries)
	}
	obj, err := f.NewObject(context.Background(), "cipher")
	if err != nil {
		t.Fatalf("NewObject recovered backup: %v", err)
	}
	reader, err := obj.Open(context.Background())
	if err != nil {
		t.Fatalf("Open recovered backup: %v", err)
	}
	got, err := io.ReadAll(reader)
	closeTestBody(t, reader)
	if err != nil || string(got) != string(content) {
		t.Fatalf("recovered content = %q, %v; want %q", got, err, content)
	}
}

func TestEndpointMustProtectAccountToken(t *testing.T) {
	tests := []struct {
		endpoint string
		valid    bool
	}{
		{endpoint: "https://api.degoo.invalid/graphql", valid: true},
		{endpoint: "http://127.0.0.1:1234/graphql", valid: true},
		{endpoint: "http://api.degoo.invalid/graphql"},
		{endpoint: "https://user:pass@api.degoo.invalid/graphql"},
		{endpoint: "https://api.degoo.invalid/graphql?token=secret"},
		{endpoint: "/graphql"},
	}
	for _, test := range tests {
		err := validateEndpoint(test.endpoint)
		if test.valid && err != nil {
			t.Errorf("validateEndpoint(%q): %v", test.endpoint, err)
		} else if !test.valid && err == nil {
			t.Errorf("validateEndpoint(%q) unexpectedly succeeded", test.endpoint)
		}
	}
}

func TestNewFsRootFileAndMissingRoot(t *testing.T) {
	ctx := context.Background()
	fileMock := newMockDegoo()
	fileMock.items["nested"] = mockItem{id: "nested", name: "nested", parentID: "vault", category: 2}
	fileMock.items["file"] = mockItem{id: "file", name: "file", parentID: "nested", category: 6, size: 3, content: []byte("abc")}
	fileFs, server, err := newTestFsAtRoot(t, fileMock, "nested/file")
	defer server.Close()
	if !errors.Is(err, fs.ErrorIsFile) {
		t.Fatalf("NewFs error = %v, want fs.ErrorIsFile", err)
	}
	if fileFs.Root() != "nested" {
		t.Fatalf("root after file lookup = %q, want nested", fileFs.Root())
	}
	if _, err := fileFs.NewObject(ctx, "file"); err != nil {
		t.Fatalf("NewObject at returned parent root: %v", err)
	}

	missingFs, missingServer, err := newTestFsAtRoot(t, newMockDegoo(), "missing/path")
	defer missingServer.Close()
	if err != nil {
		t.Fatalf("NewFs missing root: %v", err)
	}
	if err := missingFs.Mkdir(ctx, ""); err != nil {
		t.Fatalf("Mkdir configured missing root: %v", err)
	}
}
