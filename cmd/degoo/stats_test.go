//go:build linux

package degoo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/require"
)

type statsLocalOnlyFs struct {
	fs.Fs
	blocked atomic.Bool
	calls   atomic.Int32
}

func (f *statsLocalOnlyFs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	if f.blocked.Load() {
		f.calls.Add(1)
		return nil, errors.New("stats must not list the remote")
	}
	return f.Fs.List(ctx, dir)
}

func (f *statsLocalOnlyFs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if f.blocked.Load() {
		f.calls.Add(1)
		return nil, errors.New("stats must not read remote metadata")
	}
	return f.Fs.NewObject(ctx, remote)
}

func TestRuntimeStatsLocalOnly(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("requires a local test remote")
	}
	run := fstest.NewRunIndividual(t)
	remote := &statsLocalOnlyFs{Fs: run.Fremote}
	mounted := newSyncTestVFS(t, remote, true)
	file, err := mounted.Create("private-pending-name.txt")
	require.NoError(t, err)
	_, err = file.Write([]byte("pending"))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	remote.blocked.Store(true)
	defer remote.blocked.Store(false)
	started := time.Now().Add(-time.Minute)
	_, socket := newSyncTestEndpoint(t, mounted, func() { t.Fatal("stats must not refresh the remote") }, func(ctx context.Context) (any, error) {
		stats, err := collectRuntimeStats(ctx, mounted, started)
		return map[string]any{"runtime": stats}, err
	})
	for range 2 {
		code, body := syncRequestOverSocket(t, socket, http.MethodGet, "/stats", "")
		require.Equal(t, http.StatusOK, code, string(body))
		var stats struct {
			Runtime struct {
				Cache   map[string]any    `json:"cache"`
				Queue   uploadQueueStatus `json:"uploads"`
				Process struct {
					Uptime float64 `json:"uptime_seconds"`
				} `json:"process"`
			} `json:"runtime"`
		}
		require.NoError(t, json.Unmarshal(body, &stats))
		require.Equal(t, float64(7), stats.Runtime.Cache["bytes_used"])
		require.Equal(t, float64(7), stats.Runtime.Cache["bytes_dirty"])
		require.Equal(t, float64(0), stats.Runtime.Cache["bytes_evictable"])
		require.Equal(t, 1, stats.Runtime.Queue.Queued)
		require.GreaterOrEqual(t, stats.Runtime.Process.Uptime, float64(60))
		require.NotContains(t, string(body), "private-pending-name")
		require.NotContains(t, string(body), run.Fremote.Root())
	}
	require.Zero(t, remote.calls.Load(), "statistics must not call the remote")
}

func TestRequestStatsEndpoint(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("requires a local test remote")
	}
	run := fstest.NewRunIndividual(t)
	mounted := newSyncTestVFS(t, run.Fremote, false)
	_, socket := newSyncTestEndpoint(t, mounted, func() { t.Fatal("stats must not refresh the remote") }, func(context.Context) (any, error) {
		return map[string]any{"api": map[string]any{"requests": map[string]uint64{"GetFileChildren5": 7}}}, nil
	})
	code, body := syncRequestOverSocket(t, socket, http.MethodGet, "/stats", "")
	require.Equal(t, http.StatusOK, code)
	require.JSONEq(t, `{"api":{"requests":{"GetFileChildren5":7}}}`, string(body))
	stats, err := readRequestStats(context.Background(), socket)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(stats))

	code, _ = syncRequestOverSocket(t, socket, http.MethodPost, "/stats", "")
	require.Equal(t, http.StatusMethodNotAllowed, code)
	code, _ = syncRequestOverSocket(t, socket, http.MethodGet, "/stats?remote=other", "")
	require.Equal(t, http.StatusNotFound, code)
	require.NoError(t, os.Chmod(socket, 0660))
	_, err = readRequestStats(context.Background(), socket)
	require.ErrorContains(t, err, "private")
	require.NoError(t, os.Chmod(socket, 0600))
}

func TestRequestStatsUnavailable(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("requires a local test remote")
	}
	run := fstest.NewRunIndividual(t)
	_, socket := newSyncTestEndpoint(t, newSyncTestVFS(t, run.Fremote, false), func() {})
	_, err := readRequestStats(context.Background(), socket)
	require.ErrorContains(t, err, "unavailable")
	_, failedSocket := newSyncTestEndpoint(t, newSyncTestVFS(t, run.Fremote, false), func() {}, func(context.Context) (any, error) {
		return nil, errors.New("private internal failure")
	})
	code, body := syncRequestOverSocket(t, failedSocket, http.MethodGet, "/stats", "")
	require.Equal(t, http.StatusInternalServerError, code)
	require.JSONEq(t, `{"error":"mount statistics unavailable"}`, string(body))
}

func TestRequestStatsCommandRegistered(t *testing.T) {
	command, _, err := cmd.Root.Find([]string{"degoo", "stats"})
	require.NoError(t, err)
	require.Equal(t, "stats", command.Name())
	require.NotNil(t, command.Flags().Lookup("sync-socket"))
}
