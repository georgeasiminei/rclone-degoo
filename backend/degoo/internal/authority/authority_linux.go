//go:build linux

// Package authority provides a durable, single-host writer lock for a Degoo
// vault. Its authority directory must be persistent, owner-private, and outside
// disposable caches.
//
// Initialize creates the authority explicitly. Open never creates or repairs
// safety state; it waits for the existing Linux flock until the context is
// canceled. A Lease must remain open for the full coordinator lifetime, and
// callers must call Check before coordinator work to detect a replaced path.
//
// This local lock coordinates cooperating processes that share the same kernel
// lock inode. It does not fence delayed provider requests after process death
// and does not make remote publication safe by itself. Host binding assumes a
// unique, trusted /etc/machine-id; root access or a cloned machine ID can defeat
// that binding. The authority must live on a persistent local filesystem whose
// device and inode identities remain stable across normal restarts. Moving or
// copying authority state is unsupported and fails closed when detected.
//
// Initialize is a one-time setup operation for the designated authority path.
// Calling it separately at another path creates an independent lock even for
// the same vault identity, so setup must establish one canonical path before
// configuration copies are made.
package authority

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	bindingName = "binding.json"
	lockName    = "writer.lock"
	fileMode    = 0600
	dirMode     = 0700
	maxBinding  = 8 << 10
)

var (
	// ErrNotInitialized reports that explicit authority initialization is required.
	ErrNotInitialized = errors.New("writer authority is not initialized")
	// ErrBindingMismatch reports that the authority belongs to another identity.
	ErrBindingMismatch = errors.New("writer authority binding does not match")
	// ErrReplaced reports that the authority directory or lock path changed.
	ErrReplaced = errors.New("writer authority path or lock file was replaced")
	// ErrClosed reports that a closed lease was used.
	ErrClosed = errors.New("writer authority lease is closed")
)

type binding struct {
	Version       int    `json:"version"`
	AuthorityPath string `json:"authority_path"`
	OwnerUID      uint32 `json:"owner_uid"`
	MachineID     string `json:"machine_id"`
	VaultIdentity string `json:"vault_identity"`
	LockDevice    uint64 `json:"lock_device"`
	LockInode     uint64 `json:"lock_inode"`
}

type fileIdentity struct {
	dev uint64
	ino uint64
}

// Lease holds exclusive local writer authority until Close. It is safe for
// concurrent Check and Close calls.
type Lease struct {
	mu            sync.Mutex
	path          string
	ownerUID      uint32
	machineID     string
	vaultIdentity string
	lockFD        int
	dirFD         int
	dirIdentity   fileIdentity
	lockIdentity  fileIdentity
	bindingID     fileIdentity
	bindingDigest [32]byte
	closed        bool
}

// Initialize creates a new owner-private authority at authorityPath and binds
// it to the effective UID, this host's machine ID, and vaultIdentity. The path
// must be canonical and its existing parent directory must be owner-private.
// Initialization fails if the destination already exists; it never resets an
// existing authority.
func Initialize(authorityPath, vaultIdentity string) error {
	path, err := canonicalAuthorityPath(authorityPath)
	if err != nil {
		return err
	}
	if err := validateVaultIdentity(vaultIdentity); err != nil {
		return err
	}
	uid := uint32(os.Geteuid())
	machineID, err := localMachineID()
	if err != nil {
		return err
	}
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	parentFD, err := openDirectory(parentPath)
	if err != nil {
		return fmt.Errorf("open authority parent: %w", err)
	}
	defer closeFD(parentFD)
	if err := requirePrivateDirectory(parentFD, uid); err != nil {
		return fmt.Errorf("authority parent: %w", err)
	}

	tempName, err := createTemporaryAuthorityDir(parentFD)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			removeTemporaryAuthorityDir(parentFD, tempName)
		}
	}()
	tempFD, err := unix.Openat(parentFD, tempName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open temporary authority directory: %w", err)
	}
	defer closeFD(tempFD)
	if err := unix.Fchmod(tempFD, dirMode); err != nil {
		return fmt.Errorf("secure temporary authority directory: %w", err)
	}

	if err := writeNewFileAt(tempFD, lockName, nil); err != nil {
		return fmt.Errorf("create writer lock: %w", err)
	}
	lockFD, err := unix.Openat(tempFD, lockName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open initialized writer lock: %w", err)
	}
	lockStat, err := statFD(lockFD)
	_ = unix.Close(lockFD)
	if err != nil {
		return fmt.Errorf("stat initialized writer lock: %w", err)
	}
	if err := requirePrivateFile(&lockStat, uid); err != nil {
		return fmt.Errorf("initialized writer lock: %w", err)
	}
	b := binding{
		Version:       1,
		AuthorityPath: path,
		OwnerUID:      uid,
		MachineID:     machineID,
		VaultIdentity: vaultIdentity,
		LockDevice:    lockStat.Dev,
		LockInode:     lockStat.Ino,
	}
	data, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("encode authority binding: %w", err)
	}
	if err := writeNewFileAt(tempFD, bindingName, append(data, '\n')); err != nil {
		return fmt.Errorf("create authority binding: %w", err)
	}
	if err := unix.Fsync(tempFD); err != nil {
		return fmt.Errorf("sync authority directory: %w", err)
	}
	if err := unix.Renameat2(parentFD, tempName, parentFD, name, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("authority already exists: %w", err)
		}
		return fmt.Errorf("publish authority directory: %w", err)
	}
	committed = true
	if err := unix.Fsync(parentFD); err != nil {
		return fmt.Errorf("sync authority parent: %w", err)
	}
	return nil
}

// Open validates an initialized authority and acquires its non-expiring flock.
// It never creates missing files. A canceled context interrupts lock waiting;
// pausing the owning process leaves the lock held.
func Open(ctx context.Context, authorityPath, vaultIdentity string) (*Lease, error) {
	path, err := canonicalAuthorityPath(authorityPath)
	if err != nil {
		return nil, err
	}
	if err := validateVaultIdentity(vaultIdentity); err != nil {
		return nil, err
	}
	uid := uint32(os.Geteuid())
	machineID, err := localMachineID()
	if err != nil {
		return nil, err
	}
	dirFD, err := openDirectory(path)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("open authority directory: %w", ErrNotInitialized)
		}
		return nil, fmt.Errorf("open authority directory: %w", err)
	}
	cleanupDir := true
	defer func() {
		if cleanupDir {
			_ = unix.Close(dirFD)
		}
	}()
	if err := requirePrivateDirectory(dirFD, uid); err != nil {
		return nil, fmt.Errorf("authority directory: %w", err)
	}
	actual, rawBinding, bindingID, err := readBindingAt(dirFD, uid)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("read authority binding: %w", ErrNotInitialized)
		}
		return nil, fmt.Errorf("read authority binding: %w", err)
	}
	if actual.AuthorityPath != path || actual.MachineID != machineID || actual.VaultIdentity != vaultIdentity {
		return nil, fmt.Errorf("authority identity differs: %w", ErrBindingMismatch)
	}
	lockFD, err := openLockAt(dirFD, uid)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("open writer lock: %w", ErrNotInitialized)
		}
		return nil, fmt.Errorf("open writer lock: %w", err)
	}
	lockStat, err := statFD(lockFD)
	if err != nil {
		_ = unix.Close(lockFD)
		return nil, fmt.Errorf("stat writer lock: %w", err)
	}
	if identityOf(&lockStat) != (fileIdentity{dev: actual.LockDevice, ino: actual.LockInode}) {
		_ = unix.Close(lockFD)
		return nil, fmt.Errorf("writer lock inode differs from initialized authority: %w", ErrReplaced)
	}
	dirStat, err := statFD(dirFD)
	if err != nil {
		_ = unix.Close(lockFD)
		return nil, fmt.Errorf("stat authority directory: %w", err)
	}
	lease := &Lease{
		path:          path,
		ownerUID:      uid,
		machineID:     machineID,
		vaultIdentity: vaultIdentity,
		lockFD:        lockFD,
		dirFD:         dirFD,
		dirIdentity:   identityOf(&dirStat),
		lockIdentity:  identityOf(&lockStat),
		bindingID:     bindingID,
		bindingDigest: sha256.Sum256(rawBinding),
	}
	if err := lease.checkPaths(); err != nil {
		_ = unix.Close(lockFD)
		return nil, err
	}
	cleanupDir = false
	if err := acquire(ctx, lease); err != nil {
		_ = unix.Close(lockFD)
		_ = unix.Close(dirFD)
		return nil, err
	}
	if err := lease.Check(); err != nil {
		_ = unix.Flock(lockFD, unix.LOCK_UN)
		_ = unix.Close(lockFD)
		_ = unix.Close(dirFD)
		return nil, err
	}
	return lease, nil
}

// Check verifies that the canonical authority directory and lock path still
// name the same inodes acquired by Open. Call it before coordinator work.
func (l *Lease) Check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
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

// Close releases the kernel lock and closes the lease descriptors. It is
// idempotent. The persistent lock file is deliberately left in place.
func (l *Lease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	unlockErr := unix.Flock(l.lockFD, unix.LOCK_UN)
	lockErr := unix.Close(l.lockFD)
	dirErr := unix.Close(l.dirFD)
	return errors.Join(unlockErr, lockErr, dirErr)
}

func (l *Lease) checkPaths() error {
	parentFD, err := openDirectory(filepath.Dir(l.path))
	if err != nil {
		return fmt.Errorf("resolve authority parent: %w", ErrReplaced)
	}
	defer closeFD(parentFD)
	if err := requirePrivateDirectory(parentFD, l.ownerUID); err != nil {
		return fmt.Errorf("authority parent changed: %w", ErrReplaced)
	}
	pathFD, err := openDirectory(l.path)
	if err != nil {
		return fmt.Errorf("resolve authority path: %w", ErrReplaced)
	}
	defer closeFD(pathFD)
	pathStat, err := statFD(pathFD)
	if err != nil || identityOf(&pathStat) != l.dirIdentity {
		return ErrReplaced
	}
	if err := requirePrivateDirectory(pathFD, l.ownerUID); err != nil {
		return fmt.Errorf("authority directory changed: %w", ErrReplaced)
	}
	var lockStat unix.Stat_t
	if err := unix.Fstatat(pathFD, lockName, &lockStat, unix.AT_SYMLINK_NOFOLLOW); err != nil || identityOf(&lockStat) != l.lockIdentity {
		return ErrReplaced
	}
	if err := requirePrivateFile(&lockStat, l.ownerUID); err != nil {
		return fmt.Errorf("writer lock changed: %w", ErrReplaced)
	}
	return nil
}

func acquire(ctx context.Context, lease *Lease) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := lease.checkPaths(); err != nil {
			return err
		}
		err := unix.Flock(lease.lockFD, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("acquire writer lock: %w", err)
		}
		timer := time.NewTimer(75 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func canonicalAuthorityPath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return "", fmt.Errorf("authority path must be a canonical absolute non-root path")
	}
	return path, nil
}

func validateVaultIdentity(identity string) error {
	if identity == "" || len(identity) > 4096 || !utf8.ValidString(identity) || strings.IndexByte(identity, 0) >= 0 {
		return fmt.Errorf("vault identity must be non-empty valid UTF-8 of at most 4096 bytes")
	}
	return nil
}

func openDirectory(path string) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, fmt.Errorf("directory path must be canonical and absolute")
	}
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if path == string(filepath.Separator) {
		return fd, nil
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

func requirePrivateDirectory(fd int, uid uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uid || st.Mode&07777 != dirMode {
		return fmt.Errorf("directory must be owned by UID %d with mode 0700", uid)
	}
	return nil
}

func requirePrivateFile(st *unix.Stat_t, uid uint32) error {
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uid || st.Nlink != 1 || st.Mode&07777 != fileMode {
		return fmt.Errorf("file must be a single-link regular file owned by UID %d with mode 0600", uid)
	}
	return nil
}

func readBindingAt(dirFD int, uid uint32) (binding, []byte, fileIdentity, error) {
	fd, err := unix.Openat(dirFD, bindingName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return binding{}, nil, fileIdentity{}, err
	}
	defer closeFD(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return binding{}, nil, fileIdentity{}, err
	}
	if err := requirePrivateFile(&st, uid); err != nil {
		return binding{}, nil, fileIdentity{}, err
	}
	if st.Size < 2 || st.Size > maxBinding {
		return binding{}, nil, fileIdentity{}, fmt.Errorf("authority binding has invalid size")
	}
	raw, err := readFD(fd, maxBinding+1)
	if err != nil {
		return binding{}, nil, fileIdentity{}, err
	}
	var after unix.Stat_t
	if err := unix.Fstatat(dirFD, bindingName, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil || identityOf(&after) != identityOf(&st) {
		return binding{}, nil, fileIdentity{}, ErrReplaced
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var b binding
	if err := decoder.Decode(&b); err != nil {
		return binding{}, nil, fileIdentity{}, fmt.Errorf("decode authority binding: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return binding{}, nil, fileIdentity{}, fmt.Errorf("authority binding has trailing data")
	}
	if b.Version != 1 || b.AuthorityPath == "" || b.OwnerUID != uid || b.MachineID == "" || b.VaultIdentity == "" || b.LockInode == 0 {
		return binding{}, nil, fileIdentity{}, fmt.Errorf("authority binding is incomplete or unsupported")
	}
	return b, raw, identityOf(&st), nil
}

func openLockAt(dirFD int, uid uint32) (int, error) {
	fd, err := unix.Openat(dirFD, lockName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var opened, named unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := unix.Fstatat(dirFD, lockName, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil || identityOf(&opened) != identityOf(&named) {
		_ = unix.Close(fd)
		return -1, ErrReplaced
	}
	if err := requirePrivateFile(&opened, uid); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func localMachineID() (string, error) {
	etcFD, err := openDirectory("/etc")
	if err != nil {
		return "", fmt.Errorf("open /etc for machine binding: %w", err)
	}
	defer closeFD(etcFD)
	fd, err := unix.Openat(etcFD, "machine-id", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open /etc/machine-id: %w", err)
	}
	defer closeFD(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != 0 || st.Mode&0022 != 0 || st.Size < 32 || st.Size > 64 {
		return "", fmt.Errorf("/etc/machine-id must be a root-owned, non-writable regular file")
	}
	data, err := readFD(fd, 65)
	if err != nil {
		return "", fmt.Errorf("read /etc/machine-id: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if len(id) != 32 {
		return "", fmt.Errorf("/etc/machine-id must contain 32 hexadecimal characters")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", fmt.Errorf("/etc/machine-id is malformed: %w", err)
	}
	return strings.ToLower(id), nil
}

func createTemporaryAuthorityDir(parentFD int) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", err
		}
		name := ".degoo-authority-init-" + hex.EncodeToString(suffix[:])
		if err := unix.Mkdirat(parentFD, name, dirMode); err == nil {
			if err := unix.Fchmodat(parentFD, name, dirMode, 0); err != nil {
				_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
				return "", fmt.Errorf("secure temporary authority directory: %w", err)
			}
			return name, nil
		} else if !errors.Is(err, unix.EEXIST) {
			return "", fmt.Errorf("create temporary authority directory: %w", err)
		}
	}
	return "", fmt.Errorf("could not allocate a unique temporary authority directory")
}

func removeTemporaryAuthorityDir(parentFD int, name string) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		_ = unix.Unlinkat(fd, lockName, 0)
		_ = unix.Unlinkat(fd, bindingName, 0)
		_ = unix.Close(fd)
	}
	_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
}

func writeNewFileAt(dirFD int, name string, contents []byte) error {
	fd, err := unix.Openat(dirFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, fileMode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	if err := file.Chmod(fileMode); err != nil {
		_ = file.Close()
		return err
	}
	if len(contents) != 0 {
		if _, err := file.Write(contents); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func statFD(fd int) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstat(fd, &st)
	return st, err
}

func readFD(fd, limit int) ([]byte, error) {
	var out bytes.Buffer
	chunk := make([]byte, 256)
	for {
		n, err := unix.Read(fd, chunk)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return out.Bytes(), nil
		}
		if out.Len()+n > limit {
			return nil, fmt.Errorf("file exceeds %d bytes", limit)
		}
		_, _ = out.Write(chunk[:n])
	}
}

func identityOf(st *unix.Stat_t) fileIdentity {
	return fileIdentity{dev: st.Dev, ino: st.Ino}
}

func closeFD(fd int) {
	_ = unix.Close(fd)
}
