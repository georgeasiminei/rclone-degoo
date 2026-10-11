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
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscache/writeback"
	"golang.org/x/sys/unix"
)

const maxSyncRequestBytes = 64 << 10

type syncRequest struct {
	Path      *string `json:"path"`
	Recursive *bool   `json:"recursive"`
}

type syncResponse struct {
	Path      string `json:"path"`
	Refreshed bool   `json:"refreshed"`
}

type syncErrorResponse struct {
	Error string `json:"error"`
}

type syncEndpoint struct {
	server     *http.Server
	listener   *net.UnixListener
	socketPath string
	socketDev  uint64
	socketIno  uint64
	refreshMu  sync.Mutex
}

func startSyncEndpoint(socketPath string, mountedVFS *vfs.VFS, flushRaw func()) (*syncEndpoint, error) {
	if mountedVFS == nil || flushRaw == nil {
		return nil, errors.New("invalid sync endpoint configuration")
	}
	if err := validateSyncSocketParent(socketPath); err != nil {
		return nil, err
	}
	if err := removeStaleSyncSocket(socketPath); err != nil {
		return nil, err
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, errors.New("could not create sync socket")
	}
	listener.SetUnlinkOnClose(false)
	if err := unix.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		_ = removeOwnedSyncSocket(socketPath, 0, 0)
		return nil, errors.New("could not protect sync socket")
	}
	var socketStat unix.Stat_t
	if err := unix.Lstat(socketPath, &socketStat); err != nil || socketStat.Mode&unix.S_IFMT != unix.S_IFSOCK || socketStat.Uid != uint32(os.Getuid()) || socketStat.Mode&0777 != 0600 {
		_ = listener.Close()
		_ = removeOwnedSyncSocket(socketPath, 0, 0)
		return nil, errors.New("sync socket permissions could not be verified")
	}

	endpoint := &syncEndpoint{
		listener:   listener,
		socketPath: socketPath,
		socketDev:  socketStat.Dev,
		socketIno:  socketStat.Ino,
	}
	handler := &syncHandler{mountedVFS: mountedVFS, flushRaw: flushRaw, endpoint: endpoint}
	endpoint.server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = endpoint.server.Serve(listener)
	}()
	return endpoint, nil
}

func (endpoint *syncEndpoint) Close() error {
	if endpoint == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := endpoint.server.Shutdown(ctx)
	if err != nil {
		_ = endpoint.server.Close()
	}
	_ = endpoint.listener.Close()
	removeErr := removeOwnedSyncSocket(endpoint.socketPath, endpoint.socketDev, endpoint.socketIno)
	if err != nil {
		return errors.New("could not stop sync endpoint cleanly")
	}
	if removeErr != nil {
		return errors.New("could not remove sync socket")
	}
	return nil
}

type syncHandler struct {
	mountedVFS *vfs.VFS
	flushRaw   func()
	endpoint   *syncEndpoint
}

type uploadQueueStatus struct {
	Queued    int `json:"queued"`
	Uploading int `json:"uploading"`
	Retrying  int `json:"retrying"`
}

func queueStatus(ctx context.Context, mountedVFS *vfs.VFS) (uploadQueueStatus, error) {
	call := rc.Calls.Get("vfs/queue")
	if call == nil {
		return uploadQueueStatus{}, errors.New("VFS upload queue call is unavailable")
	}
	out, err := call.Fn(ctx, rc.Params{"fs": fs.ConfigString(mountedVFS.Fs())})
	if err != nil {
		return uploadQueueStatus{}, err
	}
	items, ok := out["queue"].([]writeback.QueueInfo)
	if !ok {
		return uploadQueueStatus{}, errors.New("VFS upload queue response is invalid")
	}
	var status uploadQueueStatus
	for _, item := range items {
		if item.Uploading {
			status.Uploading++
		} else {
			status.Queued++
		}
		if item.Tries > 0 && !item.Uploading || item.Tries > 1 {
			status.Retrying++
		}
	}
	return status, nil
}

func (handler *syncHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/status" && r.URL.RawQuery == "" && r.Method == http.MethodGet {
		status, err := queueStatus(r.Context(), handler.mountedVFS)
		if err != nil {
			fs.Errorf(nil, "degoo: could not inspect upload queue: %v", err)
			writeSyncError(w, http.StatusInternalServerError, "upload queue unavailable")
			return
		}
		writeSyncJSON(w, http.StatusOK, status)
		return
	}
	if r.URL.Path != "/sync" || r.URL.RawQuery != "" {
		writeSyncError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeSyncError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	request, status := decodeSyncRequest(w, r)
	if status != 0 {
		message := "invalid request"
		if status == http.StatusRequestEntityTooLarge {
			message = "request too large"
		}
		writeSyncError(w, status, message)
		return
	}
	cleanPath, err := normalizeSyncPath(*request.Path)
	if err != nil {
		writeSyncError(w, http.StatusBadRequest, "invalid path")
		return
	}

	handler.endpoint.refreshMu.Lock()
	defer handler.endpoint.refreshMu.Unlock()
	started := time.Now()
	fs.Logf(nil, "degoo: refreshing mounted remote view (path %q, recursive %t)", cleanPath, *request.Recursive)
	handler.flushRaw()
	if err := refreshVFSPath(r.Context(), handler.mountedVFS, cleanPath, *request.Recursive); err != nil {
		fs.Errorf(nil, "degoo: mounted remote refresh failed (path %q, elapsed %s): %v", cleanPath, time.Since(started).Round(time.Millisecond), err)
		writeSyncError(w, http.StatusBadGateway, "refresh failed")
		return
	}
	fs.Logf(nil, "degoo: mounted remote view refreshed (path %q, elapsed %s); pending local writes retained", cleanPath, time.Since(started).Round(time.Millisecond))
	writeSyncJSON(w, http.StatusOK, syncResponse{Path: cleanPath, Refreshed: true})
}

func decodeSyncRequest(w http.ResponseWriter, r *http.Request) (*syncRequest, int) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSyncRequestBytes))
	decoder.DisallowUnknownFields()
	var request syncRequest
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, http.StatusRequestEntityTooLarge
		}
		return nil, http.StatusBadRequest
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, http.StatusBadRequest
	}
	if request.Path == nil || request.Recursive == nil {
		return nil, http.StatusBadRequest
	}
	return &request, 0
}

func normalizeSyncPath(raw string) (string, error) {
	if raw == "/" {
		return "", nil
	}
	if strings.HasPrefix(raw, "/") {
		return "", errors.New("absolute paths are not allowed")
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "", errors.New("control characters are not allowed")
		}
	}
	if raw == "" || raw == "." {
		return "", nil
	}
	parts := strings.Split(raw, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", errors.New("parent traversal is not allowed")
		default:
			clean = append(clean, part)
		}
	}
	return strings.Join(clean, "/"), nil
}

func refreshVFSPath(ctx context.Context, mountedVFS *vfs.VFS, target string, recursive bool) error {
	forgetCall := rc.Calls.Get("vfs/forget")
	refreshCall := rc.Calls.Get("vfs/refresh")
	if forgetCall == nil || refreshCall == nil {
		return errors.New("VFS refresh calls are unavailable")
	}
	fsName := fs.ConfigString(mountedVFS.Fs())
	refresh := func(dir string, recursive bool) error {
		params := rc.Params{"fs": fsName, "recursive": "false"}
		if recursive {
			params["recursive"] = "true"
		}
		if dir != "" {
			params["dir"] = dir
		}
		out, err := refreshCall.Fn(ctx, params)
		if err != nil {
			return err
		}
		result, ok := out["result"].(map[string]string)
		if !ok || result[dir] != "OK" {
			return errors.New("VFS refresh failed")
		}
		return nil
	}
	forget := func(dir string) error {
		params := rc.Params{"fs": fsName}
		if dir != "" {
			params["dir"] = dir
		}
		_, err := forgetCall.Fn(ctx, params)
		return err
	}

	if target == "" {
		if err := forget(""); err != nil {
			return err
		}
		return refresh("", recursive)
	}

	if err := forget(target); err != nil {
		return err
	}
	parentPath := path.Dir(target)
	ancestors := []string{""}
	if parentPath != "." {
		current := ""
		for _, component := range strings.Split(parentPath, "/") {
			current = path.Join(current, component)
			ancestors = append(ancestors, current)
		}
	}
	for _, ancestor := range ancestors {
		if err := refresh(ancestor, false); err != nil {
			return err
		}
	}
	return refresh(target, recursive)
}

func validateSyncSocketParent(socketPath string) error {
	if socketPath == "" || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath || strings.ContainsRune(socketPath, '\x00') {
		return errors.New("sync socket path must be an absolute clean path")
	}
	parent := filepath.Dir(socketPath)
	components := strings.Split(strings.Trim(parent, string(filepath.Separator)), string(filepath.Separator))
	current := string(filepath.Separator)
	for _, component := range components {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		var stat unix.Stat_t
		if err := unix.Lstat(current, &stat); err != nil {
			return errors.New("sync socket parent is unavailable")
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFLNK || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return errors.New("sync socket parent contains an unsafe component")
		}
		permissions := stat.Mode & 07777
		if permissions&0022 != 0 && permissions&unix.S_ISVTX == 0 {
			return errors.New("sync socket parent contains a writable unsafe component")
		}
	}
	var parentStat unix.Stat_t
	if err := unix.Lstat(parent, &parentStat); err != nil || parentStat.Mode&unix.S_IFMT != unix.S_IFDIR || parentStat.Uid != uint32(os.Getuid()) || parentStat.Mode&07777 != 0700 {
		return errors.New("sync socket parent must be owned and private")
	}
	return nil
}

func removeStaleSyncSocket(socketPath string) error {
	var stat unix.Stat_t
	if err := unix.Lstat(socketPath, &stat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return errors.New("could not inspect sync socket path")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Getuid()) || stat.Mode&0077 != 0 {
		return errors.New("existing sync socket path is unsafe")
	}
	connection, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		return errors.New("sync socket is already active")
	}
	if !errors.Is(err, unix.ECONNREFUSED) {
		return errors.New("existing sync socket could not be verified as inactive")
	}
	if err := unix.Unlink(socketPath); err != nil {
		return errors.New("could not remove stale sync socket")
	}
	return nil
}

func removeOwnedSyncSocket(socketPath string, device, inode uint64) error {
	var stat unix.Stat_t
	if err := unix.Lstat(socketPath, &stat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Getuid()) {
		return nil
	}
	if device != 0 && (stat.Dev != device || stat.Ino != inode) {
		return nil
	}
	return unix.Unlink(socketPath)
}

func writeSyncError(w http.ResponseWriter, status int, message string) {
	writeSyncJSON(w, status, syncErrorResponse{Error: message})
}

func writeSyncJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
