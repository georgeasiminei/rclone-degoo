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
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const maxRequestStatsBytes = 64 * 1024

func addStatsCommand(root *cobra.Command) {
	var socket string
	command := &cobra.Command{
		Use: "stats", Short: "Show request and metadata-cache counters from a running vault mount.",
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
