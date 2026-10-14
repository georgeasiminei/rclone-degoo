package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransferWindowRollingRateAndIdleExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).Truncate(transferStatsBucket)
	stats := transferWindow{now: func() time.Time { return now }}

	if cumulative, rate, active := stats.snapshot(); cumulative != 0 || rate != 0 || active != 0 {
		t.Fatalf("initial stats = %d, %v, %d; want zero", cumulative, rate, active)
	}

	finish := stats.begin()
	stats.record(1000)
	if cumulative, rate, active := stats.snapshot(); cumulative != 1000 || rate <= 0 || active != 1 {
		t.Fatalf("first short transfer stats = %d, %v, %d; want bytes, nonzero rate, one active", cumulative, rate, active)
	}

	now = now.Add(time.Second)
	if _, rate, _ := stats.snapshot(); rate != 1000 {
		t.Fatalf("one-second rolling rate = %v, want 1000", rate)
	}
	now = now.Add(3900 * time.Millisecond)
	if _, rate, _ := stats.snapshot(); rate <= 0 || rate >= 1000 {
		t.Fatalf("idle rate after 4.9 seconds = %v, want a decayed positive rate", rate)
	}
	now = now.Add(200 * time.Millisecond)
	if cumulative, rate, active := stats.snapshot(); cumulative != 1000 || rate != 0 || active != 1 {
		t.Fatalf("expired stats = %d, %v, %d; want cumulative bytes, zero rate, one active", cumulative, rate, active)
	}

	finish()
	if _, _, active := stats.snapshot(); active != 0 {
		t.Fatalf("active = %d after finish, want zero", active)
	}
}

func TestTransferWindowConcurrentReadersAndSnapshots(t *testing.T) {
	stats := transferWindow{now: time.Now}
	const workers = 8
	const readsPerWorker = 2000
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			finish := stats.begin()
			defer finish()
			for range readsPerWorker {
				stats.record(1)
				_, _, _ = stats.snapshot()
			}
		}()
	}
	wait.Wait()

	cumulative, rate, active := stats.snapshot()
	if want := int64(workers * readsPerWorker); cumulative != want {
		t.Fatalf("cumulative = %d, want %d", cumulative, want)
	}
	if rate <= 0 || active != 0 {
		t.Fatalf("rate/active = %v/%d, want positive/zero", rate, active)
	}
}

func TestClientTransferStatsCountUploadPayloadOnSuccessAndFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1024); err != nil {
			t.Errorf("parse upload form: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		file, _, err := request.FormFile("file")
		if err != nil {
			t.Errorf("get uploaded file: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, err := io.Copy(io.Discard, file); err != nil {
			t.Errorf("read uploaded file: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Errorf("close uploaded file: %v", err)
		}
		if requests.Add(1) == 1 {
			writer.WriteHeader(http.StatusCreated)
			return
		}
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := New(server.Client(), &testTokens{initial: "unused"}, server.URL, "key")
	auth := AuthData{BaseURL: server.URL}
	for _, test := range []struct {
		name    string
		payload string
		wantErr bool
	}{
		{name: "success", payload: "encrypted-success"},
		{name: "failed response", payload: "encrypted-failure", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := client.Upload(context.Background(), auth, "content.bin", "checksum", int64(len(test.payload)), "application/octet-stream", strings.NewReader(test.payload))
			if (err != nil) != test.wantErr {
				t.Fatalf("Upload error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}

	stats := client.TransferStats()
	wantBytes := int64(len("encrypted-success") + len("encrypted-failure"))
	if stats.UploadBytes != wantBytes {
		t.Fatalf("upload bytes = %d, want %d (payload only)", stats.UploadBytes, wantBytes)
	}
	if stats.UploadBytesPerSecond <= 0 || stats.ActiveUploads != 0 {
		t.Fatalf("upload rate/active = %v/%d, want positive/zero", stats.UploadBytesPerSecond, stats.ActiveUploads)
	}
}

func TestClientTransferStatsActiveUploadUntilResponse(t *testing.T) {
	requestReceived := make(chan struct{})
	releaseResponse := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			t.Errorf("read upload request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		close(requestReceived)
		<-releaseResponse
		writer.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(releaseResponse) })

	client := New(server.Client(), &testTokens{initial: "unused"}, server.URL, "key")
	result := make(chan error, 1)
	go func() {
		result <- client.Upload(context.Background(), AuthData{BaseURL: server.URL}, "content.bin", "checksum", 8, "application/octet-stream", strings.NewReader("12345678"))
	}()

	select {
	case <-requestReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive the upload request")
	}
	if got := client.TransferStats().ActiveUploads; got != 1 {
		t.Fatalf("active uploads while waiting for response = %d, want 1", got)
	}

	releaseOnce.Do(func() { close(releaseResponse) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Upload: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish after response was released")
	}
	if got := client.TransferStats().ActiveUploads; got != 0 {
		t.Fatalf("active uploads after response = %d, want zero", got)
	}
}

func TestClientTransferStatsCountDownloadBodyReads(t *testing.T) {
	const payload = "encrypted-object-payload"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, payload)
	}))
	defer server.Close()

	client := New(server.Client(), &testTokens{initial: "unused"}, server.URL, "key")
	response, err := client.Download(context.Background(), server.URL, "")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := client.TransferStats().ActiveDownloads; got != 1 {
		t.Fatalf("active downloads before read = %d, want 1", got)
	}

	first := make([]byte, 4)
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatalf("read first bytes: %v", err)
	}
	if stats := client.TransferStats(); stats.DownloadBytes != int64(len(first)) || stats.ActiveDownloads != 1 {
		t.Fatalf("partial download stats = %+v, want %d bytes and one active", stats, len(first))
	}

	remaining, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read remaining bytes: %v", err)
	}
	if got := string(first) + string(remaining); got != payload {
		t.Fatalf("download body = %q, want %q", got, payload)
	}
	if stats := client.TransferStats(); stats.DownloadBytes != int64(len(payload)) || stats.DownloadBytesPerSecond <= 0 || stats.ActiveDownloads != 0 {
		t.Fatalf("complete download stats = %+v, want full payload, positive rate, zero active", stats)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close download body: %v", err)
	}
	if got := client.TransferStats().ActiveDownloads; got != 0 {
		t.Fatalf("active downloads after close = %d, want zero", got)
	}
}
