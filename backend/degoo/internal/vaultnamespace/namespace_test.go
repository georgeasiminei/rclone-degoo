package vaultnamespace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/internal/vaultindex"
	"github.com/rclone/rclone/backend/degoo/internal/vaultmanifest"
	"github.com/rclone/rclone/backend/degoo/internal/vaultpage"
)

var errNamespaceWrite = errors.New("injected namespace page write failure")

type namespaceMemoryStore struct {
	mu     sync.Mutex
	pages  map[string][]byte
	puts   int
	gets   int
	failAt int
}

func (s *namespaceMemoryStore) Get(ctx context.Context, locator string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	page, ok := s.pages[locator]
	if !ok {
		return nil, fmt.Errorf("missing generated test page %q", locator)
	}
	return append([]byte(nil), page...), nil
}

func (s *namespaceMemoryStore) Put(ctx context.Context, _ vaultpage.Identity, stored []byte, digest [32]byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if s.failAt != 0 && s.puts == s.failAt {
		return "", errNamespaceWrite
	}
	if digest != sha256.Sum256(stored) {
		return "", errors.New("repository passed an incorrect page digest")
	}
	locator := fmt.Sprintf("namespace-page-%06d", s.puts)
	s.pages[locator] = append([]byte(nil), stored...)
	return locator, nil
}

type namespaceFixture struct {
	ns       *Namespace
	index    *vaultindex.Tree
	manifest *vaultmanifest.Tree
	store    *namespaceMemoryStore
	ctx      context.Context
}

func newNamespaceFixture(t *testing.T) namespaceFixture {
	t.Helper()
	var salt [32]byte
	var vaultID [16]byte
	for i := range salt {
		salt[i] = byte(i + 7)
	}
	for i := range vaultID {
		vaultID[i] = byte(i + 41)
	}
	codec, err := vaultpage.New("namespace-test-password", salt, vaultID)
	if err != nil {
		t.Fatalf("create page codec: %v", err)
	}
	store := &namespaceMemoryStore{pages: make(map[string][]byte)}
	repo, err := vaultpage.NewRepository(codec, store)
	if err != nil {
		t.Fatalf("create page repository: %v", err)
	}
	index, err := vaultindex.New(repo)
	if err != nil {
		t.Fatalf("create index tree: %v", err)
	}
	manifest, err := vaultmanifest.New(repo)
	if err != nil {
		t.Fatalf("create manifest tree: %v", err)
	}
	var key [32]byte
	for i := range key {
		key[i] = byte(i + 1)
	}
	ns, err := New(index, key)
	if err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return namespaceFixture{ns: ns, index: index, manifest: manifest, store: store, ctx: context.Background()}
}

func namespaceID(value byte) (id [16]byte) {
	for i := range id {
		id[i] = value + byte(i)
	}
	return id
}

func namespaceTime(day int) time.Time {
	return time.Date(2026, time.October, day, 12, 30, 0, 0, time.FixedZone("test", 3*60*60))
}

func (f namespaceFixture) init(t *testing.T) Snapshot {
	t.Helper()
	s, err := f.ns.Init(f.ctx, namespaceID(0x21), namespaceTime(1))
	if err != nil {
		t.Fatalf("initialize namespace: %v", err)
	}
	return s
}

func (f namespaceFixture) manifestFor(t *testing.T, id [16]byte, data string) vaultmanifest.Manifest {
	t.Helper()
	emitted := false
	manifest, err := f.manifest.Build(f.ctx, id, func(context.Context) (vaultmanifest.Chunk, error) {
		if emitted {
			return vaultmanifest.Chunk{}, io.EOF
		}
		emitted = true
		digest := sha256.Sum256([]byte("digest:" + data))
		header := sha256.Sum256([]byte("header:" + data))
		return vaultmanifest.Chunk{Offset: 0, Size: 1, StoredSize: 128, ID: "data-" + data, Digest: digest, CryptHeader: header}, nil
	})
	if err != nil {
		t.Fatalf("build test manifest: %v", err)
	}
	return manifest
}

func (f namespaceFixture) mkdir(t *testing.T, s Snapshot, path string, day int) Snapshot {
	t.Helper()
	updated, err := f.ns.Mkdir(f.ctx, s, path, namespaceTime(day))
	if err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
	return updated
}

func (f namespaceFixture) createFile(t *testing.T, s Snapshot, path string, id [16]byte, data string, day int) Snapshot {
	t.Helper()
	content := f.manifestFor(t, id, data)
	updated, err := f.ns.CreateFile(f.ctx, s, path, content, namespaceTime(day))
	if err != nil {
		t.Fatalf("create file %q: %v", path, err)
	}
	return updated
}

func (f namespaceFixture) lookup(t *testing.T, s Snapshot, path string) Entry {
	t.Helper()
	entry, err := f.ns.Lookup(f.ctx, s, path)
	if err != nil {
		t.Fatalf("lookup %q: %v", path, err)
	}
	return entry
}

func TestNestedCreateListLookupAndLinuxNameBytes(t *testing.T) {
	f := newNamespaceFixture(t)
	s := f.init(t)
	s = f.mkdir(t, s, "one", 2)
	s = f.mkdir(t, s, "one/two", 3)
	unchanged, err := f.ns.Mkdir(f.ctx, s, "one/two", namespaceTime(8))
	if err != nil || unchanged != s {
		t.Fatalf("mkdir existing directory returned snapshot/error %v/%v, want unchanged snapshot", unchanged, err)
	}
	fileID := namespaceID(0x51)
	s = f.createFile(t, s, "one/two/file", fileID, "first", 4)

	root := f.lookup(t, s, "")
	if root.ID != s.RootID || root.Kind != Directory {
		t.Fatalf("root entry = %#v", root)
	}
	file := f.lookup(t, s, "one/two/file")
	if file.ID != fileID || file.Kind != File || file.Revision != 1 || file.Content == nil || file.Size != 1 {
		t.Fatalf("file entry = %#v", file)
	}
	var listed []Entry
	if err := f.ns.List(f.ctx, s, "one/two", func(entry Entry) error {
		listed = append(listed, entry)
		return nil
	}); err != nil {
		t.Fatalf("list nested directory: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != file.ID || listed[0].Name != "file" {
		t.Fatalf("nested listing = %#v", listed)
	}

	invalidUTF8 := string([]byte{0xff, 0xfe, 'x'})
	s = f.mkdir(t, s, invalidUTF8, 5)
	entry := f.lookup(t, s, invalidUTF8)
	if entry.Name != invalidUTF8 {
		t.Fatalf("invalid UTF-8 name changed across storage: %q", entry.Name)
	}
	found := false
	if err := f.ns.List(f.ctx, s, "", func(entry Entry) error {
		if entry.Name == invalidUTF8 {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("list root with invalid UTF-8 name: %v", err)
	}
	if !found {
		t.Fatal("root listing omitted exact invalid UTF-8 name bytes")
	}
}

func TestDirectoryRenamePreservesDescendantIDsContentAndTimes(t *testing.T) {
	f := newNamespaceFixture(t)
	s := f.init(t)
	s = f.mkdir(t, s, "parent", 2)
	s = f.mkdir(t, s, "parent/child", 3)
	fileID := namespaceID(0x62)
	s = f.createFile(t, s, "parent/child/data", fileID, "stable", 4)
	beforeParent := f.lookup(t, s, "parent")
	beforeChild := f.lookup(t, s, "parent/child")
	beforeFile := f.lookup(t, s, "parent/child/data")

	moved, err := f.ns.Rename(f.ctx, s, "parent", "renamed", beforeParent, namespaceTime(9))
	if err != nil {
		t.Fatalf("rename directory: %v", err)
	}
	if _, err := f.ns.Lookup(f.ctx, moved, "parent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old directory path error = %v", err)
	}
	afterParent := f.lookup(t, moved, "renamed")
	afterChild := f.lookup(t, moved, "renamed/child")
	afterFile := f.lookup(t, moved, "renamed/child/data")
	if afterParent.ID != beforeParent.ID || afterParent.Revision != beforeParent.Revision+1 || !afterParent.ModTime.Equal(beforeParent.ModTime) {
		t.Fatalf("moved directory metadata = %#v, before %#v", afterParent, beforeParent)
	}
	if afterChild.ID != beforeChild.ID || afterChild.Revision != beforeChild.Revision || !afterChild.ModTime.Equal(beforeChild.ModTime) {
		t.Fatalf("descendant directory changed: before %#v after %#v", beforeChild, afterChild)
	}
	if afterFile.ID != beforeFile.ID || afterFile.Revision != beforeFile.Revision || afterFile.Content == nil || beforeFile.Content == nil || *afterFile.Content != *beforeFile.Content || !afterFile.ModTime.Equal(beforeFile.ModTime) {
		t.Fatalf("descendant file changed: before %#v after %#v", beforeFile, afterFile)
	}
	if afterChild.ParentID != afterParent.ID || afterFile.ParentID != afterChild.ID {
		t.Fatalf("stable parent IDs changed: parent=%x child=%x file=%x", afterParent.ID, afterChild.ParentID, afterFile.ParentID)
	}
	if oldPathFile := f.lookup(t, s, "parent/child/data"); oldPathFile.ID != beforeFile.ID {
		t.Fatalf("directory rename mutated the original snapshot: %#v", oldPathFile)
	}
}

func TestOverwriteMakesOldObjectMutationsConflict(t *testing.T) {
	f := newNamespaceFixture(t)
	s := f.init(t)
	fileID := namespaceID(0x73)
	s = f.createFile(t, s, "file", fileID, "old", 2)
	old := f.lookup(t, s, "file")
	newContent := f.manifestFor(t, fileID, "new")
	updated, err := f.ns.ReplaceFile(f.ctx, s, "file", old, newContent, namespaceTime(3))
	if err != nil {
		t.Fatalf("replace file: %v", err)
	}
	current := f.lookup(t, updated, "file")
	if current.ID != old.ID || current.Revision != old.Revision+1 || current.Content == nil || *current.Content != newContent {
		t.Fatalf("replacement entry = %#v", current)
	}
	if previous := f.lookup(t, s, "file"); previous.Revision != old.Revision || previous.Content == nil || *previous.Content != *old.Content {
		t.Fatalf("replacement mutated old snapshot: %#v", previous)
	}
	if _, err := f.ns.Remove(f.ctx, updated, "file", old, namespaceTime(4)); !errors.Is(err, ErrConflict) {
		t.Errorf("stale remove error = %v, want ErrConflict", err)
	}
	if _, err := f.ns.ReplaceFile(f.ctx, updated, "file", old, newContent, namespaceTime(4)); !errors.Is(err, ErrConflict) {
		t.Errorf("stale replace error = %v, want ErrConflict", err)
	}
	if _, err := f.ns.Rename(f.ctx, updated, "file", "moved", old, namespaceTime(4)); !errors.Is(err, ErrConflict) {
		t.Errorf("stale rename error = %v, want ErrConflict", err)
	}
	if got := f.lookup(t, updated, "file"); got.Revision != current.Revision {
		t.Fatalf("stale operations changed latest snapshot: %#v", got)
	}
}

func TestRenameReplacementWithinAndAcrossParents(t *testing.T) {
	f := newNamespaceFixture(t)
	s := f.init(t)
	s = f.mkdir(t, s, "left", 2)
	s = f.mkdir(t, s, "right", 3)
	s = f.createFile(t, s, "left/source", namespaceID(0x34), "source", 4)
	s = f.createFile(t, s, "left/target", namespaceID(0x45), "target", 5)
	source := f.lookup(t, s, "left/source")
	target := f.lookup(t, s, "left/target")
	sameParent, err := f.ns.Rename(f.ctx, s, "left/source", "left/target", source, namespaceTime(6))
	if err != nil {
		t.Fatalf("same-parent file replacement rename: %v", err)
	}
	if got := f.lookup(t, sameParent, "left/target"); got.ID != source.ID || got.Content == nil || *got.Content != *source.Content {
		t.Fatalf("same-parent rename destination = %#v", got)
	}
	if _, err := f.ns.Lookup(f.ctx, sameParent, "left/source"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("same-parent source lookup error = %v", err)
	}
	if got := f.lookup(t, s, "left/target"); got.ID != target.ID {
		t.Fatalf("same-parent replacement mutated prior snapshot: %#v", got)
	}

	crossParent, err := f.ns.Rename(f.ctx, sameParent, "left/target", "right/moved", f.lookup(t, sameParent, "left/target"), namespaceTime(7))
	if err != nil {
		t.Fatalf("cross-parent file rename: %v", err)
	}
	if got := f.lookup(t, crossParent, "right/moved"); got.ID != source.ID {
		t.Fatalf("cross-parent destination ID = %x, want %x", got.ID, source.ID)
	}
	if got := f.lookup(t, crossParent, "right"); got.Revision < 2 || !got.ModTime.Equal(namespaceTime(7).UTC()) {
		t.Fatalf("destination parent metadata = %#v", got)
	}
	if got := f.lookup(t, crossParent, "left"); got.Revision < 3 || !got.ModTime.Equal(namespaceTime(7).UTC()) {
		t.Fatalf("source parent metadata = %#v", got)
	}

	s = f.mkdir(t, s, "left/empty-source", 8)
	s = f.mkdir(t, s, "right/empty-target", 9)
	emptySource := f.lookup(t, s, "left/empty-source")
	emptyTarget := f.lookup(t, s, "right/empty-target")
	replacedDir, err := f.ns.Rename(f.ctx, s, "left/empty-source", "right/empty-target", emptySource, namespaceTime(10))
	if err != nil {
		t.Fatalf("replace empty directory: %v", err)
	}
	if got := f.lookup(t, replacedDir, "right/empty-target"); got.ID != emptySource.ID {
		t.Fatalf("empty-directory replacement ID = %x, want %x", got.ID, emptySource.ID)
	}
	if got := f.lookup(t, s, "right/empty-target"); got.ID != emptyTarget.ID {
		t.Fatalf("empty-directory replacement changed old snapshot: %x", got.ID)
	}
}

func TestNamespaceRejectsConflictsCyclesRootsAndInvalidPaths(t *testing.T) {
	f := newNamespaceFixture(t)
	s := f.init(t)
	s = f.mkdir(t, s, "dir", 2)
	s = f.mkdir(t, s, "dir/child", 3)
	s = f.createFile(t, s, "file", namespaceID(0x56), "file", 4)
	root := f.lookup(t, s, "")
	dir := f.lookup(t, s, "dir")
	child := f.lookup(t, s, "dir/child")
	file := f.lookup(t, s, "file")

	if _, err := f.ns.Remove(f.ctx, s, "", root, namespaceTime(5)); !errors.Is(err, ErrRoot) {
		t.Errorf("remove root error = %v", err)
	}
	if _, err := f.ns.Rename(f.ctx, s, "", "renamed", root, namespaceTime(5)); !errors.Is(err, ErrRoot) {
		t.Errorf("rename root error = %v", err)
	}
	if _, err := f.ns.Remove(f.ctx, s, "dir", dir, namespaceTime(5)); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("remove nonempty directory error = %v", err)
	}
	if _, err := f.ns.Rename(f.ctx, s, "dir", "dir/child/loop", dir, namespaceTime(5)); !errors.Is(err, ErrCycle) {
		t.Errorf("move directory below itself error = %v", err)
	}
	if _, err := f.ns.Rename(f.ctx, s, "file", "dir", file, namespaceTime(5)); !errors.Is(err, ErrIsDirectory) {
		t.Errorf("file over directory error = %v", err)
	}
	if _, err := f.ns.Rename(f.ctx, s, "dir/child", "file", child, namespaceTime(5)); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("directory over file error = %v", err)
	}
	if _, err := f.ns.CreateFile(f.ctx, s, "dir", f.manifestFor(t, namespaceID(0x67), "conflict"), namespaceTime(5)); !errors.Is(err, ErrExists) {
		t.Errorf("file creation at existing directory error = %v", err)
	}
	if _, err := f.ns.Mkdir(f.ctx, s, "file", namespaceTime(5)); !errors.Is(err, ErrExists) {
		t.Errorf("mkdir at existing file error = %v", err)
	}
	if _, err := f.ns.CreateFile(f.ctx, s, "duplicate-id", f.manifestFor(t, file.ID, "duplicate"), namespaceTime(5)); !errors.Is(err, ErrExists) {
		t.Errorf("create with an existing stable ID error = %v", err)
	}
	if _, err := f.ns.Rename(f.ctx, s, "dir/child", "dir", child, namespaceTime(5)); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("replace nonempty directory error = %v", err)
	}

	for _, path := range []string{"/absolute", "trailing/", "double//slash", ".", "dir/../file", "a\x00b", string(make([]byte, MaxNameBytes+1)), string(make([]byte, MaxPathBytes+1))} {
		if _, err := f.ns.Lookup(f.ctx, s, path); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("lookup path of length %d (%q) error = %v, want ErrInvalidPath", len(path), path, err)
		}
	}
}

func TestListStopsOnCallbackErrorAndContextCancellation(t *testing.T) {
	f := newNamespaceFixture(t)
	s := f.init(t)
	for i := byte(1); i <= 3; i++ {
		s = f.createFile(t, s, fmt.Sprintf("file-%d", i), namespaceID(0x80+i), fmt.Sprintf("%d", i), int(i)+1)
	}
	wantStop := errors.New("stop listing")
	if err := f.ns.List(f.ctx, s, "", func(Entry) error { return wantStop }); !errors.Is(err, wantStop) {
		t.Fatalf("callback stop error = %v", err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	visited := 0
	err := f.ns.List(ctx, s, "", func(Entry) error {
		visited++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || visited != 1 {
		t.Fatalf("cancelled listing returned %v after %d callbacks", err, visited)
	}
}

func TestFailedMultiRecordEditLeavesOriginalSnapshotReadable(t *testing.T) {
	f := newNamespaceFixture(t)
	s := f.init(t)
	s = f.mkdir(t, s, "from", 2)
	s = f.mkdir(t, s, "to", 3)
	s = f.createFile(t, s, "from/moving", namespaceID(0x91), "moving", 4)
	moving := f.lookup(t, s, "from/moving")
	f.store.mu.Lock()
	f.store.failAt = f.store.puts + 3
	f.store.mu.Unlock()
	if _, err := f.ns.Rename(f.ctx, s, "from/moving", "to/moved", moving, namespaceTime(5)); !errors.Is(err, errNamespaceWrite) {
		t.Fatalf("injected rename error = %v, want page write failure", err)
	}
	f.store.mu.Lock()
	f.store.failAt = 0
	f.store.mu.Unlock()
	if got := f.lookup(t, s, "from/moving"); got.ID != moving.ID {
		t.Fatalf("failed edit damaged original source: %#v", got)
	}
	if _, err := f.ns.Lookup(f.ctx, s, "to/moved"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed edit changed original destination: %v", err)
	}
	from := f.lookup(t, s, "from")
	to := f.lookup(t, s, "to")
	if from.Revision != 2 || to.Revision != 1 {
		t.Fatalf("failed edit changed original directory revisions: from=%d to=%d", from.Revision, to.Revision)
	}
}

func TestLookupAndListRejectLinkAndInodeMismatches(t *testing.T) {
	f := newNamespaceFixture(t)
	s := f.init(t)
	s = f.mkdir(t, s, "entry", 2)
	entry := f.lookup(t, s, "entry")
	root, err := f.ns.load(f.ctx, s, s.RootID)
	if err != nil {
		t.Fatalf("load root inode: %v", err)
	}
	raw, err := f.index.Get(f.ctx, root.Children, f.ns.childKey(root.ID, "entry"))
	if err != nil {
		t.Fatalf("read child link: %v", err)
	}
	var l link
	if err := decode(raw, &l); err != nil {
		t.Fatalf("decode child link: %v", err)
	}
	l.Name = []byte("different")
	badLink, err := json.Marshal(l)
	if err != nil {
		t.Fatalf("encode mismatched link: %v", err)
	}
	root.Children, err = f.index.Put(f.ctx, root.Children, f.ns.childKey(root.ID, "entry"), badLink)
	if err != nil {
		t.Fatalf("write mismatched link: %v", err)
	}
	badLinkSnapshot, err := f.ns.write(f.ctx, s, root)
	if err != nil {
		t.Fatalf("stage mismatched link root: %v", err)
	}
	if _, err := f.ns.Lookup(f.ctx, badLinkSnapshot, "entry"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("lookup mismatched link error = %v", err)
	}
	if err := f.ns.List(f.ctx, badLinkSnapshot, "", func(Entry) error { return nil }); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("list mismatched link error = %v", err)
	}

	child, err := f.ns.load(f.ctx, s, entry.ID)
	if err != nil {
		t.Fatalf("load child inode: %v", err)
	}
	child.Name = []byte("renamed-without-link")
	badInodeSnapshot, err := f.ns.write(f.ctx, s, child)
	if err != nil {
		t.Fatalf("stage mismatched inode: %v", err)
	}
	if _, err := f.ns.Lookup(f.ctx, badInodeSnapshot, "entry"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("lookup mismatched inode error = %v", err)
	}
}

func TestLookupRejectsDuplicateInodeKeys(t *testing.T) {
	f := newNamespaceFixture(t)
	s := f.mkdir(t, f.init(t), "entry", 2)
	entry := f.lookup(t, s, "entry")
	if _, err := f.ns.load(f.ctx, s, entry.ID); err != nil {
		t.Fatalf("decode valid inode before mutation: %v", err)
	}
	inodeBytes, err := f.index.Get(f.ctx, s.Root, f.ns.inodeKey(entry.ID))
	if err != nil {
		t.Fatalf("read valid inode: %v", err)
	}
	malformedInode := duplicateJSONKey(t, inodeBytes, `"version":1`, `"version":0,"version":1`)
	inodeRoot, err := f.index.Put(f.ctx, s.Root, f.ns.inodeKey(entry.ID), malformedInode)
	if err != nil {
		t.Fatalf("stage authenticated duplicate-key inode: %v", err)
	}
	inodeSnapshot := s
	inodeSnapshot.Root = inodeRoot
	if _, err := f.ns.Lookup(f.ctx, inodeSnapshot, "entry"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("lookup duplicate-key inode error = %v, want ErrCorrupt", err)
	}
}

func TestLookupRejectsCaseFoldedDuplicateInodeKeys(t *testing.T) {
	f := newNamespaceFixture(t)
	fileID := namespaceID(0x83)
	s := f.createFile(t, f.init(t), "file", fileID, "data", 2)
	raw, err := f.index.Get(f.ctx, s.Root, f.ns.inodeKey(fileID))
	if err != nil {
		t.Fatalf("read valid inode: %v", err)
	}
	malformed := duplicateJSONKey(t, raw, `"size":`, `"\u017fize":0,"size":`)

	// encoding/json folds long-s to ASCII s when matching struct fields. Keep
	// the canonical field last so the ordinary decoder still produces valid data.
	var decoded inode
	decoder := json.NewDecoder(bytes.NewReader(malformed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("standard decoder rejected folded field alias: %v", err)
	}
	var valid inode
	if err := decode(raw, &valid); err != nil {
		t.Fatalf("decode original inode: %v", err)
	}
	if decoded.Size != valid.Size {
		t.Fatalf("alias changed decoded size to %d, want %d", decoded.Size, valid.Size)
	}

	root, err := f.index.Put(f.ctx, s.Root, f.ns.inodeKey(fileID), malformed)
	if err != nil {
		t.Fatalf("stage authenticated folded-alias inode: %v", err)
	}
	s.Root = root
	if _, err := f.ns.Lookup(f.ctx, s, "file"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("lookup folded-alias inode error = %v, want ErrCorrupt", err)
	}
}

func TestLookupAndListRejectDuplicateLinkKeys(t *testing.T) {
	f := newNamespaceFixture(t)
	s := f.mkdir(t, f.init(t), "entry", 2)
	root, err := f.ns.load(f.ctx, s, s.RootID)
	if err != nil {
		t.Fatalf("decode valid root before mutation: %v", err)
	}
	linkKey := f.ns.childKey(root.ID, "entry")
	linkBytes, err := f.index.Get(f.ctx, root.Children, linkKey)
	if err != nil {
		t.Fatalf("read valid link: %v", err)
	}
	var decodedLink link
	if err := decode(linkBytes, &decodedLink); err != nil {
		t.Fatalf("decode valid link before mutation: %v", err)
	}
	malformedLink := duplicateJSONKey(t, linkBytes, `"version":1`, `"version":0,"version":1`)
	root.Children, err = f.index.Put(f.ctx, root.Children, linkKey, malformedLink)
	if err != nil {
		t.Fatalf("stage authenticated duplicate-key link: %v", err)
	}
	linkSnapshot, err := f.ns.write(f.ctx, s, root)
	if err != nil {
		t.Fatalf("stage duplicate-key link root: %v", err)
	}
	if _, err := f.ns.Lookup(f.ctx, linkSnapshot, "entry"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("lookup duplicate-key link error = %v, want ErrCorrupt", err)
	}
	if err := f.ns.List(f.ctx, linkSnapshot, "", func(Entry) error { return nil }); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("list duplicate-key link error = %v, want ErrCorrupt", err)
	}
}

func TestLookupRejectsEscapedDuplicateNestedInManifestReference(t *testing.T) {
	f := newNamespaceFixture(t)
	fileID := namespaceID(0x82)
	s := f.createFile(t, f.init(t), "file", fileID, "data", 2)
	if _, err := f.ns.Lookup(f.ctx, s, "file"); err != nil {
		t.Fatalf("lookup valid file before mutation: %v", err)
	}
	raw, err := f.index.Get(f.ctx, s.Root, f.ns.inodeKey(fileID))
	if err != nil {
		t.Fatalf("read file inode: %v", err)
	}
	malformed := duplicateJSONKey(t, raw, `"generation":1`, `"generation":0,"\u0067eneration":1`)
	root, err := f.index.Put(f.ctx, s.Root, f.ns.inodeKey(fileID), malformed)
	if err != nil {
		t.Fatalf("stage authenticated nested duplicate-key inode: %v", err)
	}
	s.Root = root
	if _, err := f.ns.Lookup(f.ctx, s, "file"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("lookup escaped nested duplicate-key identity error = %v, want ErrCorrupt", err)
	}
}

func TestDecodeBoundsNamespaceJSON(t *testing.T) {
	var value any
	if err := decode(bytes.Repeat([]byte{' '}, vaultindex.MaxValueSize+1), &value); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized JSON error = %v, want ErrCorrupt", err)
	}
	deep := bytes.Repeat([]byte{'['}, maxNamespaceJSONDepth+2)
	deep = append(deep, '0')
	deep = append(deep, bytes.Repeat([]byte{']'}, maxNamespaceJSONDepth+2)...)
	if err := decode(deep, &value); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("overly nested JSON error = %v, want ErrCorrupt", err)
	}
}

func duplicateJSONKey(t *testing.T, raw []byte, old, replacement string) []byte {
	t.Helper()
	malformed := bytes.Replace(raw, []byte(old), []byte(replacement), 1)
	if bytes.Equal(malformed, raw) {
		t.Fatalf("fixture did not find JSON fragment %q", old)
	}
	return malformed
}
