//go:build linux

package degoo

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fstest"
	"github.com/stretchr/testify/require"
)

func TestRequestStatsEndpoint(t *testing.T) {
	if *fstest.RemoteName != "" {
		t.Skip("requires a local test remote")
	}
	run := fstest.NewRunIndividual(t)
	mounted := newSyncTestVFS(t, run.Fremote, false)
	_, socket := newSyncTestEndpoint(t, mounted, func() { t.Fatal("stats must not refresh the remote") }, func() any {
		return map[string]any{"api": map[string]any{"requests": map[string]uint64{"GetFileChildren5": 7}}}
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
}

func TestRequestStatsCommandRegistered(t *testing.T) {
	command, _, err := cmd.Root.Find([]string{"degoo", "stats"})
	require.NoError(t, err)
	require.Equal(t, "stats", command.Name())
	require.NotNil(t, command.Flags().Lookup("sync-socket"))
}
