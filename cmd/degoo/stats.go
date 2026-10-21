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
	"runtime"
	"time"

	"github.com/rclone/rclone/vfs"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const maxRequestStatsBytes = 64 * 1024

// collectRuntimeStats inspects local VFS state without listing remote directories
// or opening remote objects. Cached ranges exclude filesystem allocation overhead.
func collectRuntimeStats(ctx context.Context, mounted *vfs.VFS, started time.Time) (map[string]any, error) {
	disk := mounted.DiskCacheStats()
	if disk == nil {
		return nil, errors.New("VFS disk cache statistics unavailable")
	}
	cache := make(map[string]any)
	for source, target := range map[string]string{
		"bytesUsed": "bytes_used", "bytesDirty": "bytes_dirty", "bytesClean": "bytes_clean",
		"bytesEvictable": "bytes_evictable", "bytesPinned": "bytes_pinned", "files": "files",
		"filesDirty": "files_dirty", "filesClean": "files_clean", "filesEvictable": "files_evictable", "filesPinned": "files_pinned",
		"maxBytes": "max_bytes", "minFreeBytes": "min_free_bytes", "outOfSpace": "out_of_space",
		"diskTotalBytes": "disk_total_bytes", "diskFreeBytes": "disk_free_bytes",
		"maxAgeSeconds": "max_age_seconds", "pollIntervalSeconds": "poll_interval_seconds",
		"erroredFiles": "errored_files",
	} {
		if value, found := disk[source]; found {
			cache[target] = value
		}
	}
	queue, err := queueStatus(ctx, mounted)
	if err != nil {
		return nil, err
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return map[string]any{
		"cache":   cache,
		"uploads": queue,
		"process": map[string]any{
			"uptime_seconds": time.Since(started).Seconds(), "goroutines": runtime.NumGoroutine(),
			"heap_alloc_bytes": memory.HeapAlloc, "heap_sys_bytes": memory.HeapSys, "sys_bytes": memory.Sys,
		},
	}, nil
}

func addStatsCommand(root *cobra.Command) {
	var socket string
	command := &cobra.Command{
		Use: "stats", Short: "Show local cache, transfer, and request counters from a running vault mount.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			stats, err := readRequestStats(command.Context(), socket)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(command.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(stats)
		},
	}
	command.Flags().StringVar(&socket, "sync-socket", "", "Private Unix socket of the running Degoo mount (required)")
	root.AddCommand(command)
}

func readRequestStats(ctx context.Context, socket string) (json.RawMessage, error) {
	if err := validateSyncSocketParent(socket); err != nil {
		return nil, err
	}
	var info unix.Stat_t
	if err := unix.Lstat(socket, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFSOCK || info.Uid != uint32(os.Getuid()) || info.Mode&07777 != 0600 {
		return nil, errors.New("request statistics require an owner-private mount socket")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/stats", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("could not read request statistics from the running mount")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("request statistics are unavailable from this mount")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRequestStatsBytes+1))
	if err != nil || len(body) > maxRequestStatsBytes || !json.Valid(body) {
		return nil, errors.New("mount returned invalid request statistics")
	}
	return json.RawMessage(body), nil
}
