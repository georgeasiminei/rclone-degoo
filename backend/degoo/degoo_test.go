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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/pacer"
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
	nextID                       int
	failRegister                 bool
	nullRegisterAcknowledgement  bool
	falseRegisterAcknowledgement bool
	applyFalseRegisterAck        bool
	registrationVisibilityLag    time.Duration
	registerCalls                int
	blockListing                 bool
	listRetryAfter               string
	listStatusCode               int
	listCalls                    int
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
	mock.downloadCalls++
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
		if checksum != "" && mock.falseRegisterAcknowledgement && !mock.applyFalseRegisterAck {
			mock.registerCalls++
			data = map[string]any{"setUploadFile3": false}
			break
		}
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
		if checksum == "" {
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

func TestWaitForItemTimeoutDoesNotWaitOnQueuedPacerRetry(t *testing.T) {
	mock := newMockDegoo()
	mock.listRetryAfter = "1"
	f, server := newTestFs(t, mock)
	defer server.Close()
	f.pacer.SetRetries(1)
	mock.mu.Lock()
	initialListCalls := mock.listCalls
	mock.mu.Unlock()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := f.listDir(ctx, "vault"); err == nil {
			t.Fatal("throttled listing unexpectedly succeeded")
		}
	}

	started := time.Now()
	_, err := f.waitForItem(ctx, "vault", "missing", 400*time.Millisecond)
	if err == nil {
		t.Fatal("lookup succeeded for a missing item")
	}
	if delay, ok := pacer.IsRetryAfter(err); !ok || delay != time.Second {
		t.Fatalf("lookup error retry-after = %v, %t; want 1s, true", delay, ok)
	}
	if elapsed := time.Since(started); elapsed > 800*time.Millisecond {
		t.Fatalf("lookup waited for queued pacer retry: %v", elapsed)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.listCalls != initialListCalls+3 {
		t.Fatalf("listing requests = %d, want %d (two pacer setup calls and one bounded lookup)", mock.listCalls, initialListCalls+3)
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
	if err := f.waitForIDsGone(context.Background(), "vault", []string{"target"}, time.Second); err != nil {
		t.Fatalf("waitForIDsGone for absent target: %v", err)
	}
}

func TestPublishedParentDiagnosticComparesPreAndPostRename(t *testing.T) {
	mock := newMockDegoo()
	mock.items["staged-private-id"] = mockItem{
		id: "staged-private-id", name: "final-cipher-name", parentID: "other-private-parent", category: 6, size: 4,
		content: []byte("data"),
	}
	f, server := newTestFs(t, mock)
	defer server.Close()

	staged := api.Item{ID: "staged-private-id", Name: temporaryPrefix + "pending", ParentID: "vault", Size: 4}
	_, err := f.waitForPublishedObject(context.Background(), "final-cipher-name", "final-cipher-name", "vault", staged, "vault", 4, time.Millisecond)
	if err == nil {
		t.Fatal("waitForPublishedObject succeeded with an unpublished parent")
	}
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
