package degoo

import (
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
)

type mockItem struct {
	id       string
	name     string
	parentID string
	category int
	size     int64
	created  time.Time
	content  []byte
}

type mockDegoo struct {
	mu            sync.Mutex
	items         map[string]mockItem
	uploads       map[string][]byte
	nextID        int
	failRegister  bool
	corruptUpload bool
	registerSeen  chan struct{}
}

func newMockDegoo() *mockDegoo {
	return &mockDegoo{
		items:   map[string]mockItem{"vault": {id: "vault", name: "vault", category: 2}},
		uploads: make(map[string][]byte),
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
	if mock.corruptUpload && len(content) != 0 {
		content[0] ^= 0xff
	}
	mock.uploads[header.Filename] = content
	mock.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (mock *mockDegoo) download(writer http.ResponseWriter, request *http.Request) {
	id := strings.TrimPrefix(request.URL.Path, "/download/")
	mock.mu.Lock()
	item, ok := mock.items[id]
	content := append([]byte(nil), item.content...)
	mock.mu.Unlock()
	if !ok {
		http.NotFound(writer, request)
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
		idObject, _ := input.Variables["ID"].(map[string]any)
		id, _ := idObject["FileID"].(string)
		item, ok := mock.items[id]
		if !ok {
			data = map[string]any{"getOverlay4": nil}
		} else {
			data = map[string]any{"getOverlay4": mock.itemJSON(item, request)}
		}
	case "GetFileChildren5":
		parentID, _ := input.Variables["ParentID"].(string)
		items := make([]map[string]any, 0)
		for _, item := range mock.items {
			if item.parentID == parentID {
				items = append(items, mock.itemJSON(item, request))
			}
		}
		data = map[string]any{"getFileChildren5": map[string]any{"Items": items}}
	case "GetBucketWriteAuth4":
		data = map[string]any{"getBucketWriteAuth4": []any{map[string]any{"AuthData": map[string]any{
			"PolicyBase64": "policy", "Signature": "signature", "BaseURL": "http://" + request.Host + "/gcs",
			"KeyPrefix": "", "AccessKey": map[string]string{"Key": "key", "Value": "value"},
		}}}}
	case "SetUploadFile3":
		fileInfos := input.Variables["FileInfos"].([]any)
		info := fileInfos[0].(map[string]any)
		name, _ := info["Name"].(string)
		parentID, _ := info["ParentID"].(string)
		checksum, _ := info["Checksum"].(string)
		if checksum != "" && mock.failRegister {
			if mock.registerSeen != nil {
				select {
				case mock.registerSeen <- struct{}{}:
				default:
				}
			}
			writeTestJSON(writer, map[string]any{"errors": []any{map[string]string{"message": "simulated registration rejection"}}})
			return
		}
		mock.nextID++
		id := fmt.Sprintf("item-%d", mock.nextID)
		size, _ := strconv.ParseInt(fmt.Sprint(info["Size"]), 10, 64)
		createdMS, _ := strconv.ParseInt(fmt.Sprint(info["CreationTime"]), 10, 64)
		item := mockItem{id: id, name: name, parentID: parentID, category: 6, size: size, created: time.UnixMilli(createdMS)}
		if checksum != "" {
			item.content = append([]byte(nil), mock.uploads[name]...)
		}
		mock.items[id] = item
		data = map[string]any{"setUploadFile3": "OK"}
	case "SetRenameFile":
		rows := input.Variables["FileRenames"].([]any)
		row := rows[0].(map[string]any)
		id, _ := row["ID"].(string)
		name, _ := row["NewName"].(string)
		item := mock.items[id]
		item.name = name
		mock.items[id] = item
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
	t.Helper()
	server := httptest.NewServer(mock)
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
	f, err := NewFs(context.Background(), "test", root, configmap.Simple{
		"auth_file": authPath, "root_folder_id": "vault", "api_key": "test-key",
		"api_url": server.URL, "rest_url": server.URL,
	})
	return f, server, err
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
	mock.items["marker"] = mockItem{id: "marker", name: directoryMarkerName("placeholder"), parentID: "placeholder", category: 6, size: 0}
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
