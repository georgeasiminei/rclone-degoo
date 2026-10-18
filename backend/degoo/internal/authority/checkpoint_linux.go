//go:build linux

package authority

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	checkpointName       = "control.state"
	checkpointTempName   = "control.tmp"
	checkpointHeaderSize = 48
	checkpointMaxFile    = 64 << 10
	checkpointMaxPayload = checkpointMaxFile - checkpointHeaderSize
)

var checkpointMagic = [8]byte{'D', 'G', 'S', 'A', 'U', 'T', 'H', '1'}

var (
	// ErrCheckpointMissing reports that no checkpoint has been initialized.
	ErrCheckpointMissing = errors.New("writer checkpoint is missing")
	// ErrCheckpointExists reports that explicit checkpoint initialization would replace state.
	ErrCheckpointExists = errors.New("writer checkpoint already exists")
	// ErrCheckpointNotLoaded reports that SaveCheckpoint was called before LoadCheckpoint or InitializeCheckpoint.
	ErrCheckpointNotLoaded = errors.New("writer checkpoint has not been loaded")
	// ErrCheckpointTooLarge reports a checkpoint that exceeds the fixed on-disk bound.
	ErrCheckpointTooLarge = errors.New("writer checkpoint exceeds 64 KiB")
	// ErrCheckpointCorrupt reports an invalid checkpoint envelope or checksum.
	ErrCheckpointCorrupt = errors.New("writer checkpoint is corrupt")
	// ErrCheckpointUnsafe reports checkpoint files that violate private-file requirements.
	ErrCheckpointUnsafe = errors.New("writer checkpoint file is unsafe")
	// ErrCheckpointChanged reports state replaced or changed during a lease.
	ErrCheckpointChanged = errors.New("writer checkpoint changed during lease")
	// ErrCheckpointConflict reports an update based on an older checkpoint payload.
	ErrCheckpointConflict = errors.New("writer checkpoint expected payload does not match")
	// ErrCheckpointIO reports an ambiguous checkpoint filesystem operation.
	ErrCheckpointIO = errors.New("writer checkpoint I/O failed")
	// ErrCheckpointPoisoned reports that the lease cannot safely perform more work.
	ErrCheckpointPoisoned = errors.New("writer authority lease is poisoned")
)

// InitializeCheckpoint creates the first checkpoint, but never replaces one.
// A prior LoadCheckpoint returning ErrCheckpointMissing is not required, though
// initialization remains explicit and only succeeds while the state file is absent.
// The checksum detects corruption, not malicious changes or stale valid backups.
func (l *Lease) InitializeCheckpoint(payload []byte) error {
	return l.initializeCheckpoint(payload, nil)
}

func (l *Lease) initializeCheckpoint(payload []byte, afterRename func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureUsableLocked(); err != nil {
		return err
	}
	if len(payload) > checkpointMaxPayload {
		return ErrCheckpointTooLarge
	}
	if err := l.checkAuthorityLocked(); err != nil {
		return err
	}
	if l.checkpointLoaded {
		if err := l.verifyLoadedCheckpointLocked(); err != nil {
			return err
		}
		return ErrCheckpointExists
	}
	payload = bytes.Clone(payload)
	present, _, err := checkpointEntry(l.dirFD, checkpointName, l.ownerUID)
	if err != nil {
		return l.poisonCheckpointLocked(err)
	}
	if present {
		return ErrCheckpointExists
	}
	if err := l.recoverCheckpointTempLocked(); err != nil {
		return err
	}
	return l.commitCheckpointLocked(payload, true, afterRename)
}

// LoadCheckpoint reads and validates the committed checkpoint. It never creates
// missing state; callers must explicitly initialize a new vault. A valid loaded
// checkpoint is bound to this lease's file identity and contents.
func (l *Lease) LoadCheckpoint() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.beginCheckpointLocked(); err != nil {
		return nil, err
	}
	if l.checkpointLoaded {
		payload, id, digest, err := readCheckpointAt(l.dirFD, l.ownerUID)
		if err != nil {
			return nil, l.poisonCheckpointLocked(checkpointChangedOrReadError(err))
		}
		if err := l.validateLoadedCheckpointLocked(id, digest); err != nil {
			return nil, err
		}
		return payload, nil
	}
	if err := l.recoverCheckpointTempLocked(); err != nil {
		return nil, err
	}
	payload, id, digest, err := readCheckpointAt(l.dirFD, l.ownerUID)
	if err != nil {
		if errors.Is(err, ErrCheckpointMissing) {
			return nil, err
		}
		return nil, l.poisonCheckpointLocked(err)
	}
	if err := l.checkAuthorityLocked(); err != nil {
		return nil, l.poisonCheckpointLocked(err)
	}
	l.checkpointLoaded = true
	l.checkpointID = id
	l.checkpointDigest = digest
	return payload, nil
}

// SaveCheckpoint atomically replaces loaded checkpoint state. A successful
// LoadCheckpoint or InitializeCheckpoint is required first. Ambiguous I/O
// failures poison the lease until it is closed and reopened.
func (l *Lease) SaveCheckpoint(payload []byte) error {
	return l.saveCheckpoint(payload, nil)
}

// CompareAndSaveCheckpoint saves payload only if expected equals the loaded
// checkpoint payload. Comparison and replacement share the lease mutex. This
// local condition prevents independent ledger instances from losing intents;
// it is not a provider conditional-write or remote fencing primitive.
func (l *Lease) CompareAndSaveCheckpoint(expected, payload []byte) error {
	return l.saveCheckpointConditional(payload, expected, true, nil)
}

func (l *Lease) saveCheckpoint(payload []byte, afterRename func() error) error {
	return l.saveCheckpointConditional(payload, nil, false, afterRename)
}

func (l *Lease) saveCheckpointConditional(payload, expected []byte, conditional bool, afterRename func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.ensureUsableLocked(); err != nil {
		return err
	}
	if len(payload) > checkpointMaxPayload {
		return ErrCheckpointTooLarge
	}
	if err := l.checkAuthorityLocked(); err != nil {
		return err
	}
	if !l.checkpointLoaded {
		return ErrCheckpointNotLoaded
	}
	if err := l.verifyLoadedCheckpointLocked(); err != nil {
		return err
	}
	if conditional {
		if len(expected) > checkpointMaxPayload {
			return ErrCheckpointTooLarge
		}
		if sha256.Sum256(encodeCheckpoint(expected)) != l.checkpointDigest {
			return ErrCheckpointConflict
		}
	}
	payload = bytes.Clone(payload)
	if err := l.recoverCheckpointTempLocked(); err != nil {
		return err
	}
	if err := l.verifyLoadedCheckpointLocked(); err != nil {
		return err
	}
	return l.commitCheckpointLocked(payload, false, afterRename)
}

func (l *Lease) beginCheckpointLocked() error {
	if err := l.ensureUsableLocked(); err != nil {
		return err
	}
	return l.checkAuthorityLocked()
}

func (l *Lease) ensureUsableLocked() error {
	if l.closed {
		return ErrClosed
	}
	if l.poisoned != nil {
		return fmt.Errorf("%w: %w", ErrCheckpointPoisoned, l.poisoned)
	}
	return nil
}

func (l *Lease) checkLocked() error {
	if err := l.beginCheckpointLocked(); err != nil {
		return err
	}
	if l.checkpointLoaded {
		if err := l.verifyLoadedCheckpointLocked(); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lease) checkAuthorityLocked() error {
	if err := l.checkPaths(); err != nil {
		return err
	}
	actual, raw, id, err := readBindingAt(l.dirFD, l.ownerUID)
	if err != nil || id != l.bindingID || sha256.Sum256(raw) != l.bindingDigest {
		return fmt.Errorf("authority binding changed: %w", ErrReplaced)
	}
	expected := binding{Version: 1, AuthorityPath: l.path, OwnerUID: l.ownerUID, MachineID: l.machineID, VaultIdentity: l.vaultIdentity, LockDevice: l.lockIdentity.dev, LockInode: l.lockIdentity.ino}
	if actual != expected {
		return fmt.Errorf("authority binding changed: %w", ErrBindingMismatch)
	}
	return nil
}

func (l *Lease) verifyLoadedCheckpointLocked() error {
	_, id, digest, err := readCheckpointAt(l.dirFD, l.ownerUID)
	if err != nil {
		return l.poisonCheckpointLocked(checkpointChangedOrReadError(err))
	}
	return l.validateLoadedCheckpointLocked(id, digest)
}

func (l *Lease) validateLoadedCheckpointLocked(id fileIdentity, digest [32]byte) error {
	if err := l.checkAuthorityLocked(); err != nil {
		return l.poisonCheckpointLocked(err)
	}
	if id != l.checkpointID || digest != l.checkpointDigest {
		return l.poisonCheckpointLocked(ErrCheckpointChanged)
	}
	return nil
}

func (l *Lease) commitCheckpointLocked(payload []byte, initialize bool, afterRename func() error) error {
	raw := encodeCheckpoint(payload)
	if err := l.writeCheckpointTempLocked(raw); err != nil {
		return l.poisonCheckpointLocked(err)
	}
	if initialize {
		present, _, err := checkpointEntry(l.dirFD, checkpointName, l.ownerUID)
		if err != nil {
			return l.poisonCheckpointLocked(err)
		}
		if present {
			return l.poisonCheckpointLocked(ErrCheckpointChanged)
		}
		if err := unix.Renameat2(l.dirFD, checkpointTempName, l.dirFD, checkpointName, unix.RENAME_NOREPLACE); err != nil {
			return l.poisonCheckpointLocked(checkpointIOError("publish initial checkpoint", err))
		}
	} else {
		if err := l.verifyLoadedCheckpointLocked(); err != nil {
			return err
		}
		if err := unix.Renameat(l.dirFD, checkpointTempName, l.dirFD, checkpointName); err != nil {
			return l.poisonCheckpointLocked(checkpointIOError("replace checkpoint", err))
		}
	}
	if afterRename != nil {
		if err := afterRename(); err != nil {
			return l.poisonCheckpointLocked(checkpointIOError("post-rename checkpoint hook", err))
		}
	}
	if err := unix.Fsync(l.dirFD); err != nil {
		return l.poisonCheckpointLocked(checkpointIOError("sync checkpoint directory", err))
	}
	stored, id, digest, err := readCheckpointAt(l.dirFD, l.ownerUID)
	if err != nil {
		return l.poisonCheckpointLocked(err)
	}
	if !bytes.Equal(stored, payload) {
		return l.poisonCheckpointLocked(ErrCheckpointChanged)
	}
	if err := l.checkAuthorityLocked(); err != nil {
		return l.poisonCheckpointLocked(err)
	}
	l.checkpointLoaded = true
	l.checkpointID = id
	l.checkpointDigest = digest
	return nil
}

func (l *Lease) writeCheckpointTempLocked(raw []byte) error {
	if err := writeNewFileAt(l.dirFD, checkpointTempName, raw); err != nil {
		return checkpointIOError("write checkpoint temporary file", err)
	}
	present, st, err := checkpointEntry(l.dirFD, checkpointTempName, l.ownerUID)
	if err != nil {
		return err
	}
	if !present || st.Size != int64(len(raw)) {
		return ErrCheckpointChanged
	}
	return verifyCheckpointEntryID(l.dirFD, checkpointTempName, l.ownerUID, identityOf(&st))
}

func (l *Lease) recoverCheckpointTempLocked() error {
	present, named, err := checkpointEntry(l.dirFD, checkpointTempName, l.ownerUID)
	if err != nil {
		return l.poisonCheckpointLocked(err)
	}
	if !present {
		return nil
	}
	fd, err := unix.Openat(l.dirFD, checkpointTempName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return l.poisonCheckpointLocked(checkpointIOError("open checkpoint temporary file", err))
	}
	opened, statErr := statFD(fd)
	if statErr != nil {
		_ = unix.Close(fd)
		return l.poisonCheckpointLocked(checkpointIOError("stat checkpoint temporary file", statErr))
	}
	if identityOf(&opened) != identityOf(&named) || opened.Size != named.Size {
		_ = unix.Close(fd)
		return l.poisonCheckpointLocked(ErrCheckpointChanged)
	}
	if err := validateCheckpointStat(&opened, checkpointTempName, l.ownerUID); err != nil {
		_ = unix.Close(fd)
		return l.poisonCheckpointLocked(err)
	}
	if err := unix.Close(fd); err != nil {
		return l.poisonCheckpointLocked(checkpointIOError("close checkpoint temporary file", err))
	}
	if err := verifyCheckpointEntryID(l.dirFD, checkpointTempName, l.ownerUID, identityOf(&opened)); err != nil {
		return l.poisonCheckpointLocked(err)
	}
	if err := unix.Unlinkat(l.dirFD, checkpointTempName, 0); err != nil {
		return l.poisonCheckpointLocked(checkpointIOError("remove uncommitted checkpoint temporary file", err))
	}
	if err := unix.Fsync(l.dirFD); err != nil {
		return l.poisonCheckpointLocked(checkpointIOError("sync checkpoint temporary cleanup", err))
	}
	return nil
}

func readCheckpointAt(dirFD int, uid uint32) (payload []byte, identity fileIdentity, digest [32]byte, retErr error) {
	present, named, err := checkpointEntry(dirFD, checkpointName, uid)
	if err != nil {
		return nil, fileIdentity{}, [32]byte{}, err
	}
	if !present {
		return nil, fileIdentity{}, [32]byte{}, ErrCheckpointMissing
	}
	fd, err := unix.Openat(dirFD, checkpointName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fileIdentity{}, [32]byte{}, fmt.Errorf("%w: %s is a symlink", ErrCheckpointUnsafe, checkpointName)
		}
		if errors.Is(err, unix.ENOENT) {
			return nil, fileIdentity{}, [32]byte{}, ErrCheckpointChanged
		}
		return nil, fileIdentity{}, [32]byte{}, checkpointIOError("open checkpoint", err)
	}
	defer func() {
		if err := unix.Close(fd); err != nil && retErr == nil {
			retErr = checkpointIOError("close checkpoint", err)
		}
	}()
	st, err := statFD(fd)
	if err != nil {
		return nil, fileIdentity{}, [32]byte{}, checkpointIOError("stat checkpoint", err)
	}
	if err := validateCheckpointStat(&st, checkpointName, uid); err != nil {
		return nil, fileIdentity{}, [32]byte{}, err
	}
	if identityOf(&st) != identityOf(&named) {
		return nil, fileIdentity{}, [32]byte{}, ErrCheckpointChanged
	}
	if st.Size < checkpointHeaderSize {
		return nil, fileIdentity{}, [32]byte{}, ErrCheckpointCorrupt
	}
	raw, err := readFD(fd, checkpointMaxFile+1)
	if err != nil {
		return nil, fileIdentity{}, [32]byte{}, checkpointIOError("read checkpoint", err)
	}
	if len(raw) > checkpointMaxFile {
		return nil, fileIdentity{}, [32]byte{}, ErrCheckpointTooLarge
	}
	var afterFD, afterName unix.Stat_t
	if err := unix.Fstat(fd, &afterFD); err != nil {
		return nil, fileIdentity{}, [32]byte{}, checkpointIOError("restat checkpoint", err)
	}
	if err := unix.Fstatat(dirFD, checkpointName, &afterName, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fileIdentity{}, [32]byte{}, ErrCheckpointChanged
		}
		return nil, fileIdentity{}, [32]byte{}, checkpointIOError("verify checkpoint path", err)
	}
	if identityOf(&st) != identityOf(&afterFD) || identityOf(&st) != identityOf(&afterName) || int64(len(raw)) != afterFD.Size || afterFD.Size != afterName.Size {
		return nil, fileIdentity{}, [32]byte{}, ErrCheckpointChanged
	}
	if err := validateCheckpointStat(&afterFD, checkpointName, uid); err != nil {
		return nil, fileIdentity{}, [32]byte{}, err
	}
	payload, err = decodeCheckpoint(raw)
	if err != nil {
		return nil, fileIdentity{}, [32]byte{}, err
	}
	return payload, identityOf(&st), sha256.Sum256(raw), nil
}

func decodeCheckpoint(raw []byte) ([]byte, error) {
	if len(raw) < checkpointHeaderSize {
		return nil, ErrCheckpointCorrupt
	}
	if !bytes.Equal(raw[:8], checkpointMagic[:]) {
		return nil, ErrCheckpointCorrupt
	}
	payloadSize := binary.BigEndian.Uint64(raw[8:16])
	if payloadSize > checkpointMaxPayload {
		return nil, ErrCheckpointTooLarge
	}
	if uint64(len(raw)-checkpointHeaderSize) != payloadSize {
		return nil, ErrCheckpointCorrupt
	}
	payload := raw[checkpointHeaderSize:]
	digest := sha256.Sum256(payload)
	if !bytes.Equal(raw[16:checkpointHeaderSize], digest[:]) {
		return nil, ErrCheckpointCorrupt
	}
	return bytes.Clone(payload), nil
}

func encodeCheckpoint(payload []byte) []byte {
	raw := make([]byte, checkpointHeaderSize+len(payload))
	copy(raw[:8], checkpointMagic[:])
	binary.BigEndian.PutUint64(raw[8:16], uint64(len(payload)))
	digest := sha256.Sum256(payload)
	copy(raw[16:checkpointHeaderSize], digest[:])
	copy(raw[checkpointHeaderSize:], payload)
	return raw
}

func checkpointEntry(dirFD int, name string, uid uint32) (bool, unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, unix.Stat_t{}, nil
		}
		return false, unix.Stat_t{}, checkpointIOError("inspect checkpoint entry", err)
	}
	if err := validateCheckpointStat(&st, name, uid); err != nil {
		return true, st, err
	}
	return true, st, nil
}

func validateCheckpointStat(st *unix.Stat_t, name string, uid uint32) error {
	if err := requirePrivateFile(st, uid); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrCheckpointUnsafe, name, err)
	}
	if st.Size < 0 {
		return fmt.Errorf("%w: %s has negative size", ErrCheckpointCorrupt, name)
	}
	if st.Size > checkpointMaxFile {
		return fmt.Errorf("%w: %s is oversized", ErrCheckpointTooLarge, name)
	}
	return nil
}

func verifyCheckpointEntryID(dirFD int, name string, uid uint32, want fileIdentity) error {
	present, st, err := checkpointEntry(dirFD, name, uid)
	if err != nil {
		return err
	}
	if !present || identityOf(&st) != want {
		return ErrCheckpointChanged
	}
	return nil
}

func checkpointChangedOrReadError(err error) error {
	if errors.Is(err, ErrCheckpointMissing) {
		return fmt.Errorf("%w: %w", ErrCheckpointChanged, err)
	}
	return err
}

func checkpointIOError(operation string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrCheckpointIO, operation, err)
}

func (l *Lease) poisonCheckpointLocked(err error) error {
	if l.poisoned == nil {
		l.poisoned = err
	}
	return err
}
