//go:build linux

package degoo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func newSyncTestVFS(t *testing.T, remote fs.Fs, fullCache bool) *vfs.VFS {
	t.Helper()
	if fullCache {
		previousCacheDir := config.GetCacheDir()
		require.NoError(t, config.SetCacheDir(filepath.Join(t.TempDir(), "cache")))
		t.Cleanup(func() { require.NoError(t, config.SetCacheDir(previousCacheDir)) })
	}
	options := vfscommon.Opt
	options.DirCacheTime = fs.Duration(time.Hour)
	options.PollInterval = 0
	if fullCache {
		options.CacheMode = vfscommon.CacheModeFull
		options.WriteBack = fs.Duration(time.Hour)
	} else {
		options.CacheMode = vfscommon.CacheModeOff
	}
	mountedVFS := vfs.New(context.Background(), remote, &options)
	t.Cleanup(mountedVFS.Shutdown)
	return mountedVFS
}

func newSyncTestEndpoint(t *testing.T, mountedVFS *vfs.VFS, flushRaw func(), stats ...func() any) (*syncEndpoint, string) {
	t.Helper()
	tempDir := t.TempDir()
	require.NoError(t, os.Chmod(tempDir, 0700))
	parent := filepath.Join(tempDir, "private")
	require.NoError(t, os.Mkdir(parent, 0700))
	socketPath := filepath.Join(parent, "vfs-sync.sock")
	endpoint, err := startSyncEndpoint(socketPath, mountedVFS, flushRaw, stats...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, endpoint.Close()) })
	return endpoint, socketPath
}

func syncRequestOverSocket(t *testing.T, socketPath string, method, target, body string) (int, []byte) {
	t.Helper()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, err := http.NewRequest(method, "http://unix"+target, strings.NewReader(body))
	require.NoError(t, err)
	response, err := client.Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, data
}

func writeSyncTestObject(t *testing.T, run *fstest.Run, name, content string) {
	t.Helper()
	run.WriteObject(context.Background(), name, content, time.Now())
}

func TestSyncEndpointRefreshesRemoteAndPreservesPendingWrite(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("requires a local test remote")
	}
	run := fstest.NewRunIndividual(t)
	ctx := context.Background()
	require.NoError(t, run.Fremote.Mkdir(ctx, "nested"))
	writeSyncTestObject(t, run, "existing.txt", "old")
	writeSyncTestObject(t, run, "remove.txt", "remove me")
	writeSyncTestObject(t, run, "nested/old.txt", "old nested")

	mountedVFS := newSyncTestVFS(t, run.Fremote, true)
	_, err := mountedVFS.ReadDir("")
	require.NoError(t, err)
	_, err = mountedVFS.ReadDir("nested")
	require.NoError(t, err)
	oldNode, err := mountedVFS.Stat("existing.txt")
	require.NoError(t, err)
	require.EqualValues(t, 3, oldNode.Size())

	pending, err := mountedVFS.Create("pending.txt")
	require.NoError(t, err)
	_, err = pending.Write([]byte("pending local bytes"))
	require.NoError(t, err)
	require.NoError(t, pending.Close())

	writeSyncTestObject(t, run, "existing.txt", "updated remotely")
	writeSyncTestObject(t, run, "created.txt", "new remote file")
	oldObject, err := run.Fremote.NewObject(ctx, "remove.txt")
	require.NoError(t, err)
	require.NoError(t, oldObject.Remove(ctx))
	writeSyncTestObject(t, run, "nested/created.txt", "new nested file")
	oldNested, err := run.Fremote.NewObject(ctx, "nested/old.txt")
	require.NoError(t, err)
	require.NoError(t, oldNested.Remove(ctx))
	_, err = run.Fremote.List(ctx, "")
	require.NoError(t, err)

	flushes := 0
	_, socketPath := newSyncTestEndpoint(t, mountedVFS, func() { flushes++ })
	status, response := syncRequestOverSocket(t, socketPath, http.MethodPost, "/sync", `{"path":"","recursive":true}`)
	require.Equal(t, http.StatusOK, status, string(response))
	var rootResult syncResponse
	require.NoError(t, json.Unmarshal(response, &rootResult))
	require.Equal(t, syncResponse{Path: "", Refreshed: true}, rootResult)

	updatedNode, err := mountedVFS.Stat("existing.txt")
	require.NoError(t, err)
	require.EqualValues(t, len("updated remotely"), updatedNode.Size())
	_, err = mountedVFS.Stat("created.txt")
	require.NoError(t, err)
	_, err = mountedVFS.Stat("remove.txt")
	require.Error(t, err)
	_, err = mountedVFS.Stat("nested/created.txt")
	require.NoError(t, err)
	_, err = mountedVFS.Stat("nested/old.txt")
	require.Error(t, err)
	require.Equal(t, 1, flushes)

	pendingFile, err := mountedVFS.Open("pending.txt")
	require.NoError(t, err)
	pendingBytes, err := io.ReadAll(pendingFile)
	require.NoError(t, err)
	require.NoError(t, pendingFile.Close())
	require.Equal(t, "pending local bytes", string(pendingBytes))
}

func TestSyncEndpointReportsOnlyUploadCounters(t *testing.T) {
	run := fstest.NewRunIndividual(t)
	mountedVFS := newSyncTestVFS(t, run.Fremote, true)
	_, socketPath := newSyncTestEndpoint(t, mountedVFS, func() {})
	handle, err := mountedVFS.OpenFile("private-filename.txt", os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = handle.Write([]byte("generated pending data"))
	require.NoError(t, err)
	require.NoError(t, handle.Close())
	code, data := syncRequestOverSocket(t, socketPath, http.MethodGet, "/status", "")
	require.Equal(t, http.StatusOK, code)
	var status uploadQueueStatus
	require.NoError(t, json.Unmarshal(data, &status))
	require.Equal(t, uploadQueueStatus{Queued: 1}, status)
	require.NotContains(t, string(data), "private-filename")
}

func TestSyncEndpointRefreshesDeepTargetWithoutForgettingSiblings(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("requires a local test remote")
	}
	run := fstest.NewRunIndividual(t)
	ctx := context.Background()
	for _, dir := range []string{"nested", "nested/child", "nested/child/grandchild"} {
		require.NoError(t, run.Fremote.Mkdir(ctx, dir))
	}
	writeSyncTestObject(t, run, "sibling.txt", "keep this node")
	writeSyncTestObject(t, run, "nested/child/grandchild/old.txt", "old nested")

	mountedVFS := newSyncTestVFS(t, run.Fremote, false)
	for _, dir := range []string{"", "nested", "nested/child", "nested/child/grandchild"} {
		_, err := mountedVFS.ReadDir(dir)
		require.NoError(t, err)
	}
	siblingNode, err := mountedVFS.Stat("sibling.txt")
	require.NoError(t, err)

	writeSyncTestObject(t, run, "nested/child/grandchild/created.txt", "new nested file")
	require.NoError(t, run.Fremote.Mkdir(ctx, "nested/child/grandchild/deeper"))
	writeSyncTestObject(t, run, "nested/child/grandchild/deeper/new.txt", "deep remote file")
	oldNested, err := run.Fremote.NewObject(ctx, "nested/child/grandchild/old.txt")
	require.NoError(t, err)
	require.NoError(t, oldNested.Remove(ctx))
	_, err = mountedVFS.Stat("nested/child/grandchild/created.txt")
	require.Error(t, err)

	flushes := 0
	_, socketPath := newSyncTestEndpoint(t, mountedVFS, func() { flushes++ })
	status, response := syncRequestOverSocket(t, socketPath, http.MethodPost, "/sync", `{"path":"nested/child/grandchild","recursive":true}`)
	require.Equal(t, http.StatusOK, status, string(response))
	var result syncResponse
	require.NoError(t, json.Unmarshal(response, &result))
	require.Equal(t, syncResponse{Path: "nested/child/grandchild", Refreshed: true}, result)
	require.Equal(t, 1, flushes)
	_, err = mountedVFS.Stat("nested/child/grandchild/created.txt")
	require.NoError(t, err)
	_, err = mountedVFS.Stat("nested/child/grandchild/deeper/new.txt")
	require.NoError(t, err)
	_, err = mountedVFS.Stat("nested/child/grandchild/old.txt")
	require.Error(t, err)
	siblingNodeAfter, err := mountedVFS.Stat("sibling.txt")
	require.NoError(t, err)
	require.Same(t, siblingNode, siblingNodeAfter, "refreshing a nested target keeps clean sibling VFS nodes")
}

type syncFailListFs struct {
	fs.Fs
}

func (f syncFailListFs) List(context.Context, string) (fs.DirEntries, error) {
	return nil, errors.New("provider rejected listing")
}

func TestSyncEndpointDoesNotReportPartialRefreshFailureAsSuccess(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("requires a local test remote")
	}
	run := fstest.NewRunIndividual(t)
	mountedVFS := newSyncTestVFS(t, syncFailListFs{Fs: run.Fremote}, false)
	_, socketPath := newSyncTestEndpoint(t, mountedVFS, func() {})

	status, response := syncRequestOverSocket(t, socketPath, http.MethodPost, "/sync", `{"path":"","recursive":true}`)
	require.Equal(t, http.StatusBadGateway, status)
	require.Equal(t, `{"error":"refresh failed"}`+"\n", string(response))
	require.NotContains(t, string(response), "provider rejected listing")
}

func TestSyncEndpointRejectsInvalidRequests(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("requires a local test remote")
	}
	run := fstest.NewRunIndividual(t)
	require.NoError(t, run.Fremote.Mkdir(context.Background(), "nested"))
	mountedVFS := newSyncTestVFS(t, run.Fremote, false)
	flushes := 0
	_, socketPath := newSyncTestEndpoint(t, mountedVFS, func() { flushes++ })

	tests := []struct {
		name   string
		method string
		target string
		body   string
		status int
	}{
		{name: "method", method: http.MethodGet, target: "/sync", body: `{}`, status: http.StatusMethodNotAllowed},
		{name: "route", method: http.MethodPost, target: "/sync/other", body: `{}`, status: http.StatusNotFound},
		{name: "unknown field", method: http.MethodPost, target: "/sync", body: `{"path":"","recursive":true,"all":true}`, status: http.StatusBadRequest},
		{name: "missing field", method: http.MethodPost, target: "/sync", body: `{"path":""}`, status: http.StatusBadRequest},
		{name: "trailing data", method: http.MethodPost, target: "/sync", body: `{"path":"","recursive":true}{}`, status: http.StatusBadRequest},
		{name: "absolute path", method: http.MethodPost, target: "/sync", body: `{"path":"/private","recursive":true}`, status: http.StatusBadRequest},
		{name: "parent traversal", method: http.MethodPost, target: "/sync", body: `{"path":"a/../private","recursive":true}`, status: http.StatusBadRequest},
		{name: "control character", method: http.MethodPost, target: "/sync", body: "{\"path\":\"bad\\npath\",\"recursive\":true}", status: http.StatusBadRequest},
		{name: "too large", method: http.MethodPost, target: "/sync", body: `{"path":"` + strings.Repeat("x", maxSyncRequestBytes) + `","recursive":true}`, status: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, _ := syncRequestOverSocket(t, socketPath, test.method, test.target, test.body)
			require.Equal(t, test.status, status)
		})
	}
	require.Zero(t, flushes)

	status, response := syncRequestOverSocket(t, socketPath, http.MethodPost, "/sync", `{"path":"nested//.","recursive":false}`)
	require.Equal(t, http.StatusOK, status, string(response))
	var normalized syncResponse
	require.NoError(t, json.Unmarshal(response, &normalized))
	require.Equal(t, syncResponse{Path: "nested", Refreshed: true}, normalized)
}

func TestSyncEndpointSocketProtectionAndCleanup(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("requires a local test remote")
	}
	run := fstest.NewRunIndividual(t)
	writeSyncTestObject(t, run, "existing.txt", "socket fixture")
	mountedVFS := newSyncTestVFS(t, run.Fremote, false)
	tempDir := t.TempDir()
	require.NoError(t, os.Chmod(tempDir, 0700))
	parent := filepath.Join(tempDir, "private")
	require.NoError(t, os.Mkdir(parent, 0700))
	socketPath := filepath.Join(parent, "vfs-sync.sock")
	endpoint, err := startSyncEndpoint(socketPath, mountedVFS, func() {})
	require.NoError(t, err)
	var socketStat unix.Stat_t
	require.NoError(t, unix.Lstat(socketPath, &socketStat))
	require.Equal(t, uint32(0600), socketStat.Mode&0777)
	require.Equal(t, uint32(os.Getuid()), socketStat.Uid)
	_, err = startSyncEndpoint(socketPath, mountedVFS, func() {})
	require.ErrorContains(t, err, "already active")
	status, response := syncRequestOverSocket(t, socketPath, http.MethodPost, "/sync", `{"path":"","recursive":true}`)
	require.Equal(t, http.StatusOK, status, string(response))
	require.NoError(t, endpoint.Close())
	_, err = os.Lstat(socketPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	require.NoError(t, err)
	stale.SetUnlinkOnClose(false)
	require.NoError(t, os.Chmod(socketPath, 0600))
	require.NoError(t, stale.Close())
	restarted, err := startSyncEndpoint(socketPath, mountedVFS, func() {})
	require.NoError(t, err)
	require.NoError(t, restarted.Close())

	regularFile := filepath.Join(parent, "regular")
	require.NoError(t, os.WriteFile(regularFile, []byte("keep"), 0600))
	_, err = startSyncEndpoint(regularFile, mountedVFS, func() {})
	require.Error(t, err)
	contents, err := os.ReadFile(regularFile)
	require.NoError(t, err)
	require.Equal(t, "keep", string(contents))

	unsafeTempDir := t.TempDir()
	require.NoError(t, os.Chmod(unsafeTempDir, 0700))
	unsafeParent := filepath.Join(unsafeTempDir, "not-private")
	require.NoError(t, os.Mkdir(unsafeParent, 0755))
	require.Error(t, validateSyncSocketParent(filepath.Join(unsafeParent, "socket")))

	symlinkTempDir := t.TempDir()
	require.NoError(t, os.Chmod(symlinkTempDir, 0700))
	symlinkParent := filepath.Join(symlinkTempDir, "link")
	require.NoError(t, os.Symlink(parent, symlinkParent))
	require.Error(t, validateSyncSocketParent(filepath.Join(symlinkParent, "socket")))

	staleListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	require.NoError(t, err)
	staleListener.SetUnlinkOnClose(false)
	require.NoError(t, os.Chmod(socketPath, 0660))
	require.NoError(t, staleListener.Close())
	_, err = startSyncEndpoint(socketPath, mountedVFS, func() {})
	require.Error(t, err, "a non-private stale socket must not be removed")
	require.FileExists(t, socketPath)
}
