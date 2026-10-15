//go:build linux

package degoo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
)

const updateLease = 2 * time.Minute

type updateControl struct {
	mountedVFS *vfs.VFS
	mu         sync.Mutex
	preparing  bool
	closed     bool
	cancel     context.CancelFunc
	token      string
	release    func()
	timer      *time.Timer
	expires    time.Time
	lease      time.Duration
}

type prepareUpdateRequest struct {
	WriterTimeout *float64 `json:"writer_timeout"`
	UploadTimeout *float64 `json:"upload_timeout"`
}

type updateTokenRequest struct {
	Token string `json:"token"`
}

func decodeUpdateRequest(w http.ResponseWriter, r *http.Request, request any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSyncRequestBytes))
	decoder.DisallowUnknownFields()
	if decoder.Decode(request) != nil || decoder.Decode(new(any)) != io.EOF {
		writeSyncError(w, http.StatusBadRequest, "invalid update request")
		return false
	}
	return true
}

func (control *updateControl) busy() bool {
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.token != "" && !time.Now().Before(control.expires) {
		control.clearLocked()
	}
	return control.preparing || control.token != ""
}

func (control *updateControl) clearLocked() {
	if control.timer != nil {
		control.timer.Stop()
		control.timer = nil
	}
	if control.release != nil {
		control.release()
		control.release = nil
	}
	control.token = ""
}

func (control *updateControl) Close() {
	control.mu.Lock()
	defer control.mu.Unlock()
	control.closed = true
	if control.cancel != nil {
		control.cancel()
	}
	control.clearLocked()
}

func (control *updateControl) renewLocked() {
	if control.timer != nil {
		control.timer.Stop()
	}
	control.expires = time.Now().Add(control.lease)
	token := control.token
	control.timer = time.AfterFunc(control.lease, func() {
		control.mu.Lock()
		defer control.mu.Unlock()
		if control.token == token && !time.Now().Before(control.expires) {
			control.clearLocked()
		}
	})
}

func (control *updateControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeSyncError(w, http.StatusNotFound, "not found")
		return
	}
	switch r.URL.Path {
	case "/update/prepare", "/update/renew", "/update/cancel":
	default:
		writeSyncError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeSyncError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.Path == "/update/prepare" {
		control.prepare(w, r)
		return
	}
	var request updateTokenRequest
	if !decodeUpdateRequest(w, r, &request) {
		return
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.token != "" && !time.Now().Before(control.expires) {
		control.clearLocked()
	}
	if request.Token == "" || request.Token != control.token || control.closed {
		writeSyncError(w, http.StatusConflict, "update preparation expired or belongs to another request")
		return
	}
	if r.URL.Path == "/update/renew" {
		control.renewLocked()
		writeSyncJSON(w, http.StatusOK, map[string]bool{"renewed": true})
		return
	}
	control.clearLocked()
	writeSyncJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

func (control *updateControl) prepare(w http.ResponseWriter, r *http.Request) {
	var request prepareUpdateRequest
	if !decodeUpdateRequest(w, r, &request) {
		return
	}
	if request.WriterTimeout == nil || request.UploadTimeout == nil ||
		*request.WriterTimeout <= 0 || *request.WriterTimeout > 3600 ||
		*request.UploadTimeout < 0 || *request.UploadTimeout > 86400 {
		writeSyncError(w, http.StatusBadRequest, "invalid update timeout")
		return
	}
	writerTimeout := time.Duration(*request.WriterTimeout * float64(time.Second))
	uploadTimeout := time.Duration(*request.UploadTimeout * float64(time.Second))
	if writerTimeout <= 0 || (*request.UploadTimeout > 0 && uploadTimeout == 0) {
		writeSyncError(w, http.StatusBadRequest, "invalid update timeout")
		return
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		writeSyncError(w, http.StatusInternalServerError, "could not create update preparation")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	control.mu.Lock()
	if control.token != "" && !time.Now().Before(control.expires) {
		control.clearLocked()
	}
	if control.preparing || control.token != "" || control.closed {
		control.mu.Unlock()
		writeSyncError(w, http.StatusConflict, "another update preparation is active")
		return
	}
	control.preparing = true
	control.cancel = cancel
	control.mu.Unlock()
	defer func() {
		control.mu.Lock()
		control.preparing = false
		control.cancel = nil
		control.mu.Unlock()
	}()

	writerCtx, writerCancel := context.WithTimeout(ctx, writerTimeout)
	release, err := control.mountedVFS.PrepareMaintenance(writerCtx)
	writerCancel()
	if err != nil {
		writeUpdatePreparationError(w, err)
		return
	}
	retained := false
	defer func() {
		if !retained {
			release()
		}
	}()
	if uploadTimeout > 0 {
		uploadCtx, uploadCancel := context.WithTimeout(ctx, uploadTimeout)
		err = waitUpdateUploads(uploadCtx, control.mountedVFS)
		if err == nil {
			err = control.mountedVFS.FlushMaintenance(uploadCtx)
		}
		uploadCancel()
		if err != nil {
			writeUpdatePreparationError(w, err)
			return
		}
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if control.closed || ctx.Err() != nil {
		writeSyncError(w, http.StatusConflict, "update preparation cancelled")
		return
	}
	control.token = hex.EncodeToString(secret[:])
	control.release = release
	retained = true
	control.renewLocked()
	writeSyncJSON(w, http.StatusOK, map[string]any{"token": control.token, "prepared": true})
}

func writeUpdatePreparationError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, vfs.EBUSY) {
		writeSyncError(w, http.StatusConflict, "update preparation timed out or the mount is busy; mount remains running")
		return
	}
	fs.Errorf(nil, "degoo: update preparation failed: %v", err)
	writeSyncError(w, http.StatusInternalServerError, "could not flush or inspect the local cache; mount remains running")
}

func waitUpdateUploads(ctx context.Context, mounted *vfs.VFS) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		status, err := queueStatus(ctx, mounted)
		if err != nil {
			return err
		}
		if status.Queued == 0 && status.Uploading == 0 && status.Retrying == 0 {
			pending, err := mounted.HasPendingWrites(ctx)
			if err != nil {
				return err
			}
			if !pending {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
