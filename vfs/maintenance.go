package vfs

import (
	"context"
	"sync"
)

// maintenanceHandle tracks an open handle until it is closed.
type maintenanceHandle struct {
	vfs  *VFS
	once sync.Once
}

func (h *maintenanceHandle) release() {
	if h == nil || h.vfs == nil {
		return
	}
	h.once.Do(func() {
		vfs := h.vfs
		vfs.maintenanceMu.Lock()
		vfs.maintenanceHandles--
		if vfs.maintenance {
			vfs.notifyMaintenanceLocked()
		}
		vfs.maintenanceMu.Unlock()
	})
}

// beginMaintenanceOperation admits an operation unless maintenance is active.
func (vfs *VFS) beginMaintenanceOperation() (func(), error) {
	vfs.maintenanceMu.Lock()
	if vfs.maintenance {
		vfs.maintenanceMu.Unlock()
		return nil, EBUSY
	}
	vfs.maintenanceOps++
	vfs.maintenanceMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			vfs.maintenanceMu.Lock()
			vfs.maintenanceOps--
			if vfs.maintenance {
				vfs.notifyMaintenanceLocked()
			}
			vfs.maintenanceMu.Unlock()
		})
	}, nil
}

// trackMaintenanceHandle records a handle before its creating operation exits.
func (vfs *VFS) trackMaintenanceHandle(handle Handle) {
	var tracked *maintenanceHandle
	switch h := handle.(type) {
	case *DirHandle:
		tracked = &maintenanceHandle{vfs: vfs}
		h.maintenance = tracked
	case *ReadFileHandle:
		tracked = &maintenanceHandle{vfs: vfs}
		h.maintenance = tracked
	case *WriteFileHandle:
		tracked = &maintenanceHandle{vfs: vfs}
		h.maintenance = tracked
	case *RWFileHandle:
		tracked = &maintenanceHandle{vfs: vfs}
		h.maintenance = tracked
	default:
		return
	}
	vfs.maintenanceMu.Lock()
	vfs.maintenanceHandles++
	vfs.maintenanceMu.Unlock()
}

func (vfs *VFS) notifyMaintenanceLocked() {
	close(vfs.maintenanceChanged)
	vfs.maintenanceChanged = make(chan struct{})
}

func (vfs *VFS) endMaintenance() {
	vfs.maintenanceMu.Lock()
	vfs.maintenance = false
	vfs.maintenanceMu.Unlock()
}

// PrepareMaintenance blocks new opens and mutations, waits for existing
// operations and handles to finish, and syncs local cached data and metadata.
// The returned function releases the barrier and is safe to call more than once.
// If preparation fails, the barrier is released before the error is returned.
func (vfs *VFS) PrepareMaintenance(ctx context.Context) (release func(), err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	vfs.maintenanceMu.Lock()
	if vfs.maintenance {
		vfs.maintenanceMu.Unlock()
		return nil, EBUSY
	}
	vfs.maintenance = true
	vfs.maintenanceChanged = make(chan struct{})
	vfs.maintenanceMu.Unlock()

	var once sync.Once
	release = func() {
		once.Do(vfs.endMaintenance)
	}

	for {
		vfs.maintenanceMu.Lock()
		if vfs.maintenanceOps == 0 && vfs.maintenanceHandles == 0 {
			vfs.maintenanceMu.Unlock()
			break
		}
		changed := vfs.maintenanceChanged
		vfs.maintenanceMu.Unlock()

		select {
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		case <-changed:
		}
	}

	if err = vfs.FlushMaintenance(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// FlushMaintenance syncs cached data and metadata while a maintenance barrier
// is held. It can be called again after waiting for optional remote writeback.
func (vfs *VFS) FlushMaintenance(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	vfs.maintenanceMu.Lock()
	active := vfs.maintenance
	drained := vfs.maintenanceOps == 0 && vfs.maintenanceHandles == 0
	vfs.maintenanceMu.Unlock()
	if !active || !drained {
		return EBUSY
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if vfs.cache == nil {
		return nil
	}
	return vfs.cache.Sync(ctx)
}

// HasPendingWrites reports whether any local cached item still needs writeback.
func (vfs *VFS) HasPendingWrites(ctx context.Context) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if vfs.cache == nil {
		return false, nil
	}
	return vfs.cache.HasDirty(ctx)
}
