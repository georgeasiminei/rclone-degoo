package degoo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

type putOutcome struct {
	remote string
	object fs.Object
	err    error
}

func startPuts(f *Fs, inputs map[string][]byte, contexts map[string]context.Context) <-chan putOutcome {
	outcomes := make(chan putOutcome, len(inputs))
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(len(inputs))
	for remote, content := range inputs {
		remote, content := remote, append([]byte(nil), content...)
		ctx := contexts[remote]
		if ctx == nil {
			ctx = context.Background()
		}
		go func() {
			ready.Done()
			<-start
			obj, err := f.Put(ctx, bytes.NewReader(content), object.NewStaticObjectInfo(remote, time.Now(), int64(len(content)), true, nil, f))
			outcomes <- putOutcome{remote: remote, object: obj, err: err}
		}()
	}
	ready.Wait()
	close(start)
	return outcomes
}

func collectPuts(t *testing.T, outcomes <-chan putOutcome, count int) map[string]putOutcome {
	t.Helper()
	got := make(map[string]putOutcome, count)
	for range count {
		select {
		case outcome := <-outcomes:
			got[outcome.remote] = outcome
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for concurrent uploads")
		}
	}
	return got
}

func TestConcurrentUploadsBatchAuthorizationAndRegistration(t *testing.T) {
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()
	mock.mu.Lock()
	mock.blockUpload = true
	mock.uploadStarted = make(chan struct{}, 2)
	mock.uploadRelease = make(chan struct{})
	initialAuthCalls := mock.authorizationRequestCalls
	initialRegisterCalls := mock.registerRequestCalls
	mock.mu.Unlock()

	outcomes := startPuts(f, map[string][]byte{
		"cipher-a": []byte("generated payload alpha"),
		"cipher-b": []byte("generated payload beta"),
	}, nil)

	for range 2 {
		select {
		case <-mock.uploadStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("both authorized uploads did not reach storage")
		}
	}
	close(mock.uploadRelease)
	results := collectPuts(t, outcomes, 2)
	for _, outcome := range results {
		if outcome.err != nil || outcome.object == nil {
			t.Fatalf("Put(%q) = %v, %v", outcome.remote, outcome.object, outcome.err)
		}
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if got := mock.authorizationRequestCalls - initialAuthCalls; got != 1 {
		t.Errorf("authorization POSTs = %d, want 1 for two uploads", got)
	}
	if got := mock.authorizationRows; got != 2 {
		t.Errorf("authorization rows = %d, want 2", got)
	}
	if got := mock.registerRequestCalls - initialRegisterCalls; got != 1 {
		t.Errorf("registration POSTs = %d, want 1 for two uploads", got)
	}
	if mock.registerCalls != 2 {
		t.Errorf("registered upload rows = %d, want 2", mock.registerCalls)
	}
	if mock.authMappingErrors != 0 {
		t.Errorf("upload authorizations mapped to the wrong staged names %d times", mock.authMappingErrors)
	}
}

func TestBatchedAuthorizationPartialFailureKeepsResultMapping(t *testing.T) {
	mock := newMockDegoo()
	mock.failFirstWriteAuth = true
	f, server := newTestFs(t, mock)
	defer server.Close()

	results := collectPuts(t, startPuts(f, map[string][]byte{
		"cipher-a": []byte("generated payload alpha"),
		"cipher-b": []byte("generated payload beta"),
	}, nil), 2)
	succeeded := 0
	failed := 0
	for _, outcome := range results {
		if outcome.err == nil && outcome.object != nil {
			succeeded++
		} else if outcome.err != nil {
			failed++
		} else {
			t.Errorf("Put(%q) returned neither an object nor an error", outcome.remote)
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("partial authorization outcomes: %d succeeded, %d failed; want one each", succeeded, failed)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.authorizationRequestCalls != 1 || mock.authorizationRows != 2 {
		t.Fatalf("authorization calls/rows = %d/%d, want 1/2", mock.authorizationRequestCalls, mock.authorizationRows)
	}
	if mock.authMappingErrors != 0 {
		t.Fatalf("successful row used another upload's authorization %d times", mock.authMappingErrors)
	}
	if mock.registerCalls != 1 {
		t.Fatalf("registered upload rows = %d, want only the authorized upload", mock.registerCalls)
	}
}

func TestBatchedAuthorizationCancellationDoesNotCancelOtherUpload(t *testing.T) {
	mock := newMockDegoo()
	mock.authRequestSeen = make(chan int, 1)
	mock.authRequestRelease = make(chan struct{})
	mock.authRequestCanceled = make(chan struct{}, 1)
	f, server := newTestFs(t, mock)
	defer server.Close()

	cancelledCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := startPuts(f, map[string][]byte{
		"cipher-a": []byte("generated payload alpha"),
		"cipher-b": []byte("generated payload beta"),
	}, map[string]context.Context{"cipher-a": cancelledCtx})
	select {
	case rows := <-mock.authRequestSeen:
		if rows != 2 {
			t.Fatalf("batched authorization rows = %d, want 2", rows)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("batched authorization request did not start")
	}
	cancel()
	close(mock.authRequestRelease)

	got := collectPuts(t, results, 2)
	if got["cipher-a"].err == nil {
		t.Fatal("canceled Put unexpectedly succeeded")
	}
	if got["cipher-b"].err != nil || got["cipher-b"].object == nil {
		t.Fatalf("uncanceled Put = %v, %v; want success", got["cipher-b"].object, got["cipher-b"].err)
	}
	select {
	case <-mock.authRequestCanceled:
		t.Fatal("one caller cancellation canceled the shared authorization request")
	default:
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.registerCalls != 1 {
		t.Fatalf("registration rows = %d, want only the surviving upload", mock.registerCalls)
	}
}

func TestAmbiguousRegistrationBatchReconcilesEachStagedName(t *testing.T) {
	mock := newMockDegoo()
	mock.falseRegisterAcknowledgement = true
	mock.applyFalseRegisterAck = true
	f, server := newTestFs(t, mock)
	defer server.Close()

	results := collectPuts(t, startPuts(f, map[string][]byte{
		"cipher-a": []byte("generated payload alpha"),
		"cipher-b": []byte("generated payload beta"),
	}, nil), 2)
	for _, outcome := range results {
		if outcome.err != nil || outcome.object == nil {
			t.Fatalf("Put(%q) failed after reconciling the applied batch: %v", outcome.remote, outcome.err)
		}
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.registerRequestCalls != 1 || mock.registerCalls != 2 {
		t.Fatalf("registration POSTs/rows = %d/%d, want 1/2", mock.registerRequestCalls, mock.registerCalls)
	}
	if len(mock.uploads) != 2 {
		t.Fatalf("stored upload count = %d, want 2", len(mock.uploads))
	}
}

func TestRemoveWaitsForSamePathUpdate(t *testing.T) {
	mock := newMockDegoo()
	f, server := newTestFs(t, mock)
	defer server.Close()

	ctx := context.Background()
	original := []byte("previous encrypted version")
	old, err := f.Put(ctx, bytes.NewReader(original), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(original)), true, nil, f))
	if err != nil {
		t.Fatalf("initial Put: %v", err)
	}
	mock.mu.Lock()
	mock.authRequestSeen = make(chan int, 1)
	mock.authRequestRelease = make(chan struct{})
	mock.mu.Unlock()

	replacement := []byte("replacement encrypted version")
	updateDone := make(chan error, 1)
	go func() {
		updateDone <- old.(*Object).Update(ctx, bytes.NewReader(replacement), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(replacement)), true, nil, f))
	}()
	select {
	case <-mock.authRequestSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("update authorization did not start")
	}
	removeDone := make(chan error, 1)
	go func() { removeDone <- old.Remove(ctx) }()
	time.Sleep(50 * time.Millisecond)
	close(mock.authRequestRelease)
	if err := <-updateDone; err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := <-removeDone; err != nil && !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("Remove(old version) = %v, want stale-version result", err)
	}

	current, err := f.NewObject(ctx, "cipher")
	if err != nil {
		t.Fatalf("active replacement was removed: %v", err)
	}
	body, err := current.Open(ctx)
	if err != nil {
		t.Fatalf("Open replacement: %v", err)
	}
	got, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("replacement content = %q, %v; want %q", got, readErr, replacement)
	}
}

func TestUploadDoesNotReplaceTargetThatAppearsBeforePublication(t *testing.T) {
	mock := newMockDegoo()
	external := []byte("independently added encrypted content")
	mock.injectTargetOnDownload = true
	mock.injectedTargetName = "cipher"
	mock.injectedTargetContent = external
	f, server := newTestFs(t, mock)
	defer server.Close()

	content := []byte("generated upload content")
	_, err := f.Put(context.Background(), bytes.NewReader(content), object.NewStaticObjectInfo("cipher", time.Now(), int64(len(content)), true, nil, f))
	if err == nil || !strings.Contains(err.Error(), "target appeared during upload") {
		t.Fatalf("Put after target appeared during upload = %v, want publication conflict", err)
	}
	current, err := f.NewObject(context.Background(), "cipher")
	if err != nil {
		t.Fatalf("external target was lost: %v", err)
	}
	body, err := current.Open(context.Background())
	if err != nil {
		t.Fatalf("Open external target: %v", err)
	}
	got, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr != nil || !bytes.Equal(got, external) {
		t.Fatalf("external target content = %q, %v; want %q", got, readErr, external)
	}
}
