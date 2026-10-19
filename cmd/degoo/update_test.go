//go:build linux

package degoo

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscache/writeback"
	"github.com/stretchr/testify/require"
)

func prepareTestUpdate(t *testing.T, socket string, timeout float64) string {
	t.Helper()
	body, err := json.Marshal(map[string]float64{"writer_timeout": timeout, "upload_timeout": 0})
	require.NoError(t, err)
	status, response := syncRequestOverSocket(t, socket, http.MethodPost, "/update/prepare", string(body))
	require.Equal(t, http.StatusOK, status, string(response))
	var prepared struct {
		Token    string `json:"token"`
		Prepared bool   `json:"prepared"`
	}
	require.NoError(t, json.Unmarshal(response, &prepared))
	require.True(t, prepared.Prepared)
	require.Len(t, prepared.Token, 64)
	return prepared.Token
}

func TestUpdatePreparationTimeoutKeepsExistingWriterUsable(t *testing.T) {
	run := fstest.NewRunIndividual(t)
	mounted := newSyncTestVFS(t, run.Fremote, true)
	writer, err := mounted.Create("pending.txt")
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	_, err = writer.Write([]byte("before"))
	require.NoError(t, err)
	_, socket := newSyncTestEndpoint(t, mounted, func() {})
	status, response := syncRequestOverSocket(t, socket, http.MethodPost, "/update/prepare", `{"writer_timeout":0.05,"upload_timeout":0}`)
	require.Equal(t, http.StatusConflict, status, string(response))
	_, err = writer.Write([]byte("after"))
	require.NoError(t, err)
	another, err := mounted.Create("another.txt")
	require.NoError(t, err)
	require.NoError(t, another.Close())
}

func TestUpdatePreparationRetainsQueuedWritesAndRejectsNewOpens(t *testing.T) {
	run := fstest.NewRunIndividual(t)
	mounted := newSyncTestVFS(t, run.Fremote, true)
	writer, err := mounted.Create("pending.txt")
	require.NoError(t, err)
	_, err = writer.Write([]byte("saved locally"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	_, socket := newSyncTestEndpoint(t, mounted, func() {})
	token := prepareTestUpdate(t, socket, 1)
	status, response := syncRequestOverSocket(t, socket, http.MethodPost, "/sync", `{"path":"","recursive":true}`)
	require.Equal(t, http.StatusConflict, status, string(response))
	_, err = mounted.OpenFile("new.txt", os.O_CREATE|os.O_WRONLY, 0600)
	require.ErrorIs(t, err, vfs.EBUSY)
	status, response = syncRequestOverSocket(t, socket, http.MethodGet, "/status", "")
	require.Equal(t, http.StatusOK, status, string(response))
	var queue uploadQueueStatus
	require.NoError(t, json.Unmarshal(response, &queue))
	require.Equal(t, 1, queue.Queued)
	body, err := json.Marshal(map[string]string{"token": token})
	require.NoError(t, err)
	status, response = syncRequestOverSocket(t, socket, http.MethodPost, "/update/cancel", string(body))
	require.Equal(t, http.StatusOK, status, string(response))
	next, err := mounted.Create("new.txt")
	require.NoError(t, err)
	require.NoError(t, next.Close())
}

func TestUpdateUploadTimeoutReleasesAdmission(t *testing.T) {
	run := fstest.NewRunIndividual(t)
	mounted := newSyncTestVFS(t, run.Fremote, true)
	writer, err := mounted.Create("pending.txt")
	require.NoError(t, err)
	_, err = writer.Write([]byte("saved locally"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	_, socket := newSyncTestEndpoint(t, mounted, func() {})
	status, response := syncRequestOverSocket(t, socket, http.MethodPost, "/update/prepare", `{"writer_timeout":1,"upload_timeout":0.05}`)
	require.Equal(t, http.StatusConflict, status, string(response))
	next, err := mounted.Create("new.txt")
	require.NoError(t, err)
	require.NoError(t, next.Close())
}

func TestUpdatePreparationLeaseAndOwner(t *testing.T) {
	run := fstest.NewRunIndividual(t)
	mounted := newSyncTestVFS(t, run.Fremote, true)
	endpoint, socket := newSyncTestEndpoint(t, mounted, func() {})
	endpoint.updates.lease = 50 * time.Millisecond
	token := prepareTestUpdate(t, socket, 1)
	status, response := syncRequestOverSocket(t, socket, http.MethodPost, "/update/cancel", `{"token":"0000000000000000000000000000000000000000000000000000000000000000"}`)
	require.Equal(t, http.StatusConflict, status, string(response))
	body, err := json.Marshal(map[string]string{"token": token})
	require.NoError(t, err)
	status, response = syncRequestOverSocket(t, socket, http.MethodPost, "/update/renew", string(body))
	require.Equal(t, http.StatusOK, status, string(response))
	require.Eventually(t, func() bool {
		handle, openErr := mounted.Create("after-expiry.txt")
		if openErr != nil {
			return false
		}
		_ = handle.Close()
		return true
	}, time.Second, 10*time.Millisecond)
}

func TestUpdateEndpointRejectsUnboundedAndUnknownRequests(t *testing.T) {
	run := fstest.NewRunIndividual(t)
	mounted := newSyncTestVFS(t, run.Fremote, true)
	_, socket := newSyncTestEndpoint(t, mounted, func() {})
	for _, body := range []string{
		`{}`, `{"writer_timeout":-1,"upload_timeout":0}`,
		`{"writer_timeout":3601,"upload_timeout":0}`,
		`{"writer_timeout":1,"upload_timeout":86401}`,
		`{"writer_timeout":1,"upload_timeout":0,"extra":true}`,
		`{"writer_timeout":1,"upload_timeout":0} {}`,
	} {
		status, response := syncRequestOverSocket(t, socket, http.MethodPost, "/update/prepare", body)
		require.Equal(t, http.StatusBadRequest, status, string(response))
	}
}

func TestUpdateWaitForUploadsFinishesAfterPublication(t *testing.T) {
	run := fstest.NewRunIndividual(t)
	mounted := newSyncTestVFS(t, run.Fremote, true)
	writer, err := mounted.Create("pending.txt")
	require.NoError(t, err)
	_, err = writer.Write([]byte("generated upload"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	queue, err := rc.Calls.Get("vfs/queue").Fn(context.Background(), rc.Params{"fs": fs.ConfigString(mounted.Fs())})
	require.NoError(t, err)
	items := queue["queue"].([]writeback.QueueInfo)
	require.Len(t, items, 1)
	_, err = rc.Calls.Get("vfs/queue-set-expiry").Fn(context.Background(), rc.Params{"fs": fs.ConfigString(mounted.Fs()), "id": int64(items[0].ID), "expiry": float64(-1)})
	require.NoError(t, err)
	_, socket := newSyncTestEndpoint(t, mounted, func() {})
	status, response := syncRequestOverSocket(t, socket, http.MethodPost, "/update/prepare", `{"writer_timeout":1,"upload_timeout":3}`)
	require.Equal(t, http.StatusOK, status, string(response))
	pending, err := mounted.HasPendingWrites(context.Background())
	require.NoError(t, err)
	require.False(t, pending)
	object, err := run.Fremote.NewObject(context.Background(), "pending.txt")
	require.NoError(t, err)
	require.Equal(t, []byte("generated upload"), read(t, object))
}

func TestUpdateClientDisconnectReleasesPreparation(t *testing.T) {
	run := fstest.NewRunIndividual(t)
	mounted := newSyncTestVFS(t, run.Fremote, true)
	writer, err := mounted.Create("pending.txt")
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	_, socket := newSyncTestEndpoint(t, mounted, func() {})
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/update/prepare", strings.NewReader(`{"writer_timeout":60,"upload_timeout":0}`))
	require.NoError(t, err)
	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Eventually(t, func() bool {
		handle, err := mounted.Create("after-disconnect.txt")
		if err != nil {
			return false
		}
		_ = handle.Close()
		return true
	}, time.Second, 10*time.Millisecond)
}
