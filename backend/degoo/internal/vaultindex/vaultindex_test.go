package vaultindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/rclone/rclone/backend/degoo/internal/vaultpage"
)

var errInjectedCreate = errors.New("injected create failure")

type storedPage struct {
	identity vaultpage.Identity
	bytes    []byte
}

// indexMemoryStore retains every immutable page so superseded roots remain
// readable throughout the copy-on-write tests.
type indexMemoryStore struct {
	objects   map[string]storedPage
	puts      int
	gets      int
	failAtPut int
}

func newIndexMemoryStore() *indexMemoryStore {
	return &indexMemoryStore{objects: make(map[string]storedPage)}
}

func (s *indexMemoryStore) Get(ctx context.Context, locator string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.gets++
	object, ok := s.objects[locator]
	if !ok {
		return nil, fmt.Errorf("missing generated page %q", locator)
	}
	return bytes.Clone(object.bytes), nil
}

func (s *indexMemoryStore) Put(ctx context.Context, identity vaultpage.Identity, stored []byte, digest [32]byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.puts++
	if s.failAtPut != 0 && s.puts == s.failAtPut {
		return "", errInjectedCreate
	}
	if digest != sha256.Sum256(stored) {
		return "", errors.New("repository passed a wrong page digest")
	}
	locator := fmt.Sprintf("object-%06d", s.puts)
	if _, exists := s.objects[locator]; exists {
		return "", errors.New("store attempted to reuse an immutable locator")
	}
	s.objects[locator] = storedPage{identity: identity, bytes: bytes.Clone(stored)}
	return locator, nil
}

type indexFixture struct {
	tree  *Tree
	repo  *vaultpage.Repository
	codec *vaultpage.Codec
	store *indexMemoryStore
}

func newIndexFixture(t *testing.T) indexFixture {
	t.Helper()
	var salt [32]byte
	var vaultID [16]byte
	for i := range salt {
		salt[i] = byte(i + 3)
	}
	for i := range vaultID {
		vaultID[i] = byte(i + 31)
	}
	codec, err := vaultpage.New("index-test-password", salt, vaultID)
	if err != nil {
		t.Fatalf("create test codec: %v", err)
	}
	store := newIndexMemoryStore()
	repo, err := vaultpage.NewRepository(codec, store)
	if err != nil {
		t.Fatalf("create test repository: %v", err)
	}
	tree, err := New(repo)
	if err != nil {
		t.Fatalf("create test tree: %v", err)
	}
	return indexFixture{tree: tree, repo: repo, codec: codec, store: store}
}

func mustEmpty(t *testing.T, tree *Tree) vaultpage.Reference {
	t.Helper()
	root, err := tree.Empty(context.Background())
	if err != nil {
		t.Fatalf("create empty root: %v", err)
	}
	return root
}

func keyWithUint64(value uint64) Key {
	var key Key
	for i := 0; i < 8; i++ {
		key[len(key)-1-i] = byte(value >> (8 * i))
	}
	return key
}

func putValue(t *testing.T, tree *Tree, root vaultpage.Reference, key Key, value []byte) vaultpage.Reference {
	t.Helper()
	updated, err := tree.Put(context.Background(), root, key, value)
	if err != nil {
		t.Fatalf("put key %x: %v", key, err)
	}
	return updated
}

func pageAt(t *testing.T, tree *Tree, root vaultpage.Reference) page {
	t.Helper()
	n, err := tree.readPage(context.Background(), root, nil, true)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	return *n
}

func TestEmptyPutGetDeleteAndImmutableOldRoot(t *testing.T) {
	fixture := newIndexFixture(t)
	ctx := context.Background()
	root0 := mustEmpty(t, fixture.tree)
	key := keyWithUint64(7)
	root1 := putValue(t, fixture.tree, root0, key, []byte("first"))
	root2 := putValue(t, fixture.tree, root1, key, []byte("second"))
	if root0 == root1 || root1 == root2 {
		t.Fatal("a changed write reused the previous root reference")
	}
	for _, tc := range []struct {
		root vaultpage.Reference
		want string
	}{{root0, ""}, {root1, "first"}, {root2, "second"}} {
		got, err := fixture.tree.Get(ctx, tc.root, key)
		if tc.root == root0 {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("empty old root lookup error = %v", err)
			}
			continue
		}
		if err != nil || string(got) != tc.want {
			t.Fatalf("old-root value = %q, %v; want %q", got, err, tc.want)
		}
	}
	noOp, err := fixture.tree.Put(ctx, root2, key, []byte("second"))
	if err != nil || noOp != root2 {
		t.Fatalf("idempotent put root/error = %v/%v, want original root", noOp, err)
	}
	missing := keyWithUint64(8)
	noDelete, err := fixture.tree.Delete(ctx, root2, missing)
	if err != nil || noDelete != root2 {
		t.Fatalf("missing delete root/error = %v/%v, want original root", noDelete, err)
	}
	deleted, err := fixture.tree.Delete(ctx, root2, key)
	if err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if _, err := fixture.tree.Get(ctx, deleted, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted lookup error = %v", err)
	}
	if got, err := fixture.tree.Get(ctx, root2, key); err != nil || string(got) != "second" {
		t.Fatalf("delete changed old root: value=%q error=%v", got, err)
	}
}

func TestLongPrefixCompressionAndMaximumLookupReads(t *testing.T) {
	fixture := newIndexFixture(t)
	root := mustEmpty(t, fixture.tree)
	var first, second Key
	for i := range first {
		first[i], second[i] = 0x8a, 0x8a
	}
	first[len(first)-1] = 0x10
	second[len(second)-1] = 0x20
	root = putValue(t, fixture.tree, root, first, []byte("left"))
	root = putValue(t, fixture.tree, root, second, []byte("right"))
	n := pageAt(t, fixture.tree, root)
	if n.Kind != branchPage || n.Depth != 62 || n.Prefix != strings.Repeat("8a", 31) {
		t.Fatalf("long-prefix root = kind %q depth %d prefix %q", n.Kind, n.Depth, n.Prefix)
	}
	if got, err := fixture.tree.Get(context.Background(), root, first); err != nil || string(got) != "left" {
		t.Fatalf("first lookup = %q, %v", got, err)
	}

	// One branch at each nibble depth plus a terminal leaf reaches the bound.
	deepRoot := mustEmpty(t, fixture.tree)
	var deepest Key
	for depth := 0; depth < keyNibbles; depth++ {
		key := deepest
		setNibble(&key, depth, 1)
		deepRoot = putValue(t, fixture.tree, deepRoot, key, []byte{byte(depth)})
	}
	deepRoot = putValue(t, fixture.tree, deepRoot, deepest, []byte("deep"))
	fixture.store.gets = 0
	got, err := fixture.tree.Get(context.Background(), deepRoot, deepest)
	if err != nil || string(got) != "deep" {
		t.Fatalf("deep lookup = %q, %v", got, err)
	}
	if fixture.store.gets != keyNibbles+1 {
		t.Fatalf("deep lookup read %d pages, want at most %d", fixture.store.gets, keyNibbles+1)
	}
}

func setNibble(key *Key, depth, value int) {
	if depth%2 == 0 {
		key[depth/2] = (key[depth/2] & 0x0f) | byte(value<<4)
	} else {
		key[depth/2] = (key[depth/2] & 0xf0) | byte(value)
	}
}

func TestLeafSplitsOnRecordAndEncodedByteBounds(t *testing.T) {
	t.Run("record count", func(t *testing.T) {
		fixture := newIndexFixture(t)
		root := mustEmpty(t, fixture.tree)
		for i := uint64(0); i < MaxLeafRecords+1; i++ {
			root = putValue(t, fixture.tree, root, keyWithUint64(i), []byte{byte(i)})
		}
		if n := pageAt(t, fixture.tree, root); n.Kind != branchPage {
			t.Fatalf("65 records left root as %q", n.Kind)
		}
		assertTreeRecords(t, fixture.tree, root, MaxLeafRecords+1)
	})

	t.Run("encoded bytes", func(t *testing.T) {
		fixture := newIndexFixture(t)
		root := mustEmpty(t, fixture.tree)
		value := bytes.Repeat([]byte{0x5c}, MaxValueSize)
		const count = 20 // Below the record cap but too large for one page.
		for i := uint64(0); i < count; i++ {
			root = putValue(t, fixture.tree, root, keyWithUint64(i), value)
		}
		if n := pageAt(t, fixture.tree, root); n.Kind != branchPage {
			t.Fatalf("byte-overflow records left root as %q", n.Kind)
		}
		assertTreeRecords(t, fixture.tree, root, count)
	})
}

func TestDeleteMergesBoundedLeafChildrenAndKeepsOldRoot(t *testing.T) {
	fixture := newIndexFixture(t)
	ctx := context.Background()
	root := mustEmpty(t, fixture.tree)
	var left, right Key
	for i := range left {
		left[i], right[i] = 0x33, 0x33
	}
	left[len(left)-1] = 0x11
	right[len(right)-1] = 0x22
	root = putValue(t, fixture.tree, root, left, []byte("left"))
	root = putValue(t, fixture.tree, root, right, []byte("right"))
	oldRoot := root
	if pageAt(t, fixture.tree, root).Kind != branchPage {
		t.Fatal("expected two leaves under a branch before deletion")
	}
	fixture.store.gets = 0
	merged, err := fixture.tree.Delete(ctx, root, left)
	if err != nil {
		t.Fatalf("delete from branch: %v", err)
	}
	if fixture.store.gets > 16 {
		t.Fatalf("delete inspected %d pages at one branch", fixture.store.gets)
	}
	if n := pageAt(t, fixture.tree, merged); n.Kind != leafPage || len(n.Records) != 1 {
		t.Fatalf("merged root kind/count = %q/%d", n.Kind, len(n.Records))
	}
	if got, err := fixture.tree.Get(ctx, merged, right); err != nil || string(got) != "right" {
		t.Fatalf("merged root lookup = %q, %v", got, err)
	}
	if got, err := fixture.tree.Get(ctx, oldRoot, left); err != nil || string(got) != "left" {
		t.Fatalf("previous branch root changed: %q, %v", got, err)
	}
}

func TestDeleteInspectsAtMostSixteenChildrenAtOneBranch(t *testing.T) {
	fixture := newIndexFixture(t)
	root := mustEmpty(t, fixture.tree)
	keys := make([]Key, 16)
	for i := range keys {
		keys[i][0] = byte(i << 4)
		root = putValue(t, fixture.tree, root, keys[i], []byte{byte(i)})
	}
	if n := pageAt(t, fixture.tree, root); n.Kind != branchPage {
		t.Fatalf("root kind = %q, want branch", n.Kind)
	}
	fixture.store.gets = 0
	if _, err := fixture.tree.Delete(context.Background(), root, keys[7]); err != nil {
		t.Fatalf("delete from 16-way branch: %v", err)
	}
	// One root read and at most one read for each of its sixteen child refs.
	if fixture.store.gets > 17 {
		t.Fatalf("delete read %d pages at a 16-child branch", fixture.store.gets)
	}
}

func TestMaximumDepthRefusesAnUnsplitablePage(t *testing.T) {
	fixture := newIndexFixture(t)
	key := keyWithUint64(1)
	fixture.tree.maxPayload = 1 // Force even a singleton leaf over its encoded budget.
	puts := fixture.store.puts
	_, _, err := fixture.tree.build(context.Background(), []record{{Key: key}}, keyPrefix(key, keyNibbles), keyNibbles)
	if !errors.Is(err, ErrDepthLimit) {
		t.Fatalf("unsplittable depth-64 leaf error = %v", err)
	}
	if fixture.store.puts != puts {
		t.Fatal("unsplittable leaf created a remote page")
	}
}

func TestFailedSplitLeavesOldRootReadableAndDoesNotRetry(t *testing.T) {
	fixture := newIndexFixture(t)
	ctx := context.Background()
	root := mustEmpty(t, fixture.tree)
	for i := uint64(0); i < MaxLeafRecords; i++ {
		root = putValue(t, fixture.tree, root, keyWithUint64(i), []byte{byte(i)})
	}
	oldRoot := root
	putsBefore, objectsBefore := fixture.store.puts, len(fixture.store.objects)
	fixture.store.failAtPut = putsBefore + 2 // One split child stages; the next create fails.
	updated, err := fixture.tree.Put(ctx, root, keyWithUint64(MaxLeafRecords), []byte("new"))
	if !errors.Is(err, errInjectedCreate) {
		t.Fatalf("split failure = %v, want injected error", err)
	}
	if updated != (vaultpage.Reference{}) {
		t.Fatal("failed update returned a publishable root")
	}
	if fixture.store.puts != putsBefore+2 || len(fixture.store.objects) != objectsBefore+1 {
		t.Fatalf("failed split calls/objects = %d/%d, want one staged page and one failed create", fixture.store.puts-putsBefore, len(fixture.store.objects)-objectsBefore)
	}
	for i := uint64(0); i < MaxLeafRecords; i++ {
		got, err := fixture.tree.Get(ctx, oldRoot, keyWithUint64(i))
		if err != nil || !bytes.Equal(got, []byte{byte(i)}) {
			t.Fatalf("old key %d after failed COW = %x, %v", i, got, err)
		}
	}
}

func TestWalkThousandRecordsAndRandomPageIdentities(t *testing.T) {
	fixture := newIndexFixture(t)
	ctx := context.Background()
	root := mustEmpty(t, fixture.tree)
	const count = 1000
	for i := uint64(0); i < count; i++ {
		key := keyWithUint64(i * 7919)
		root = putValue(t, fixture.tree, root, key, []byte(fmt.Sprintf("value-%d", i)))
	}
	seen := make(map[Key]string, count)
	err := fixture.tree.Walk(ctx, root, func(key Key, value []byte) error {
		if _, duplicate := seen[key]; duplicate {
			return errors.New("walk yielded duplicate key")
		}
		seen[key] = string(value)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(seen) != count {
		t.Fatalf("walk yielded %d records, want %d", len(seen), count)
	}
	for i := uint64(0); i < count; i++ {
		if got := seen[keyWithUint64(i*7919)]; got != fmt.Sprintf("value-%d", i) {
			t.Fatalf("walk value %d = %q", i, got)
		}
	}
	identities := make(map[[16]byte]struct{}, len(fixture.store.objects))
	for _, object := range fixture.store.objects {
		if object.identity.Role != vaultpage.RoleIndex || object.identity.Generation != 1 || allZero(object.identity.LogicalID[:]) {
			t.Fatalf("invalid created identity: %+v", object.identity)
		}
		if _, duplicate := identities[object.identity.LogicalID]; duplicate {
			t.Fatalf("logical page ID reused: %x", object.identity.LogicalID)
		}
		identities[object.identity.LogicalID] = struct{}{}
	}
}

func assertTreeRecords(t *testing.T, tree *Tree, root vaultpage.Reference, want int) {
	t.Helper()
	count := 0
	var previous Key
	first := true
	err := tree.Walk(context.Background(), root, func(key Key, _ []byte) error {
		if !first && bytes.Compare(previous[:], key[:]) >= 0 {
			return errors.New("walk order is not strictly increasing")
		}
		first = false
		previous = key
		count++
		return nil
	})
	if err != nil || count != want {
		t.Fatalf("walk count/error = %d/%v, want %d", count, err, want)
	}
}

func TestWalkCancellationStopsBeforeNextCallback(t *testing.T) {
	fixture := newIndexFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	root := mustEmpty(t, fixture.tree)
	root = putValue(t, fixture.tree, root, keyWithUint64(1), []byte("one"))
	root = putValue(t, fixture.tree, root, keyWithUint64(2), []byte("two"))
	callbacks := 0
	err := fixture.tree.Walk(ctx, root, func(Key, []byte) error {
		callbacks++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || callbacks != 1 {
		t.Fatalf("walk cancellation error/callbacks = %v/%d", err, callbacks)
	}
}

func TestMalformedAuthenticatedPagesAndReferencesRejected(t *testing.T) {
	fixture := newIndexFixture(t)
	ctx := context.Background()
	validKey := keyWithUint64(1)
	goodLeaf := page{Version: indexVersion, Kind: leafPage, Prefix: keyPrefix(validKey, keyNibbles), Depth: keyNibbles, Records: []record{{Key: validKey, Value: []byte("ok")}}, Children: []*vaultpage.Reference{}}
	goodPayload, err := json.Marshal(goodLeaf)
	if err != nil {
		t.Fatal(err)
	}
	goodRef := fixture.rawPage(t, goodPayload)
	if got, err := fixture.tree.Get(ctx, goodRef, validKey); err != nil || string(got) != "ok" {
		t.Fatalf("valid hand-built leaf = %q, %v", got, err)
	}

	badPrefix := goodLeaf
	badPrefix.Prefix = strings.Repeat("f", keyNibbles)
	badSort := page{Version: indexVersion, Kind: leafPage, Prefix: "", Depth: 0, Records: []record{{Key: keyWithUint64(2)}, {Key: keyWithUint64(1)}}, Children: []*vaultpage.Reference{}}
	badMix := goodLeaf
	badMix.Children = emptyChildren()
	badBranch := page{Version: indexVersion, Kind: branchPage, Prefix: "", Depth: 0, Records: []record{}, Children: make([]*vaultpage.Reference, 15)}
	badChildRef := page{Version: indexVersion, Kind: branchPage, Prefix: "", Depth: 0, Records: []record{}, Children: emptyChildren()}
	wrongRole := goodRef
	wrongRole.Identity.Role = vaultpage.RoleManifest
	badChildRef.Children[0] = &wrongRole
	badChildRef.Children[1] = &goodRef
	duplicateChild := page{Version: indexVersion, Kind: branchPage, Prefix: "", Depth: 0, Records: []record{}, Children: emptyChildren()}
	duplicateChild.Children[0] = &goodRef
	duplicateChild.Children[1] = &goodRef
	for name, malformed := range map[string]page{
		"leaf prefix mismatch": badPrefix,
		"unsorted records":     badSort,
		"mixed shapes":         badMix,
		"branch child count":   badBranch,
		"wrong child role":     badChildRef,
		"duplicate child ref":  duplicateChild,
	} {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(malformed)
			if err != nil {
				t.Fatal(err)
			}
			ref := fixture.rawPage(t, payload)
			if _, err := fixture.tree.Get(ctx, ref, validKey); !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("malformed page error = %v", err)
			}
		})
	}

	// A structurally valid child must still match the authenticated parent slot.
	var otherKey Key
	otherKey[0] = 0xf0
	wrongChild := page{Version: indexVersion, Kind: leafPage, Prefix: keyPrefix(otherKey, keyNibbles), Depth: keyNibbles, Records: []record{{Key: otherKey, Value: []byte("wrong slot")}}, Children: []*vaultpage.Reference{}}
	wrongChildBytes, _ := json.Marshal(wrongChild)
	wrongChildRef := fixture.rawPage(t, wrongChildBytes)
	children := emptyChildren()
	children[0] = &wrongChildRef
	children[1] = &goodRef
	parent := page{Version: indexVersion, Kind: branchPage, Prefix: "", Depth: 0, Records: []record{}, Children: children}
	parentBytes, _ := json.Marshal(parent)
	parentRef := fixture.rawPage(t, parentBytes)
	if _, err := fixture.tree.Get(ctx, parentRef, validKey); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("wrong child-slot prefix error = %v", err)
	}
}

func (f indexFixture) rawPage(t *testing.T, payload []byte) vaultpage.Reference {
	t.Helper()
	var id [16]byte
	id[0] = 0xfe
	for i := 0; i < 8; i++ {
		id[15-i] = byte(uint64(len(f.store.objects)+1) >> (8 * i))
	}
	identity := vaultpage.Identity{Role: vaultpage.RoleIndex, LogicalID: id, Generation: 1}
	stored, digest, err := f.codec.Seal(identity, payload)
	if err != nil {
		t.Fatalf("seal test page: %v", err)
	}
	locator, err := f.store.Put(context.Background(), identity, stored, digest)
	if err != nil {
		t.Fatalf("store test page: %v", err)
	}
	return vaultpage.Reference{Locator: locator, Identity: identity, Digest: digest}
}

func TestMissingCorruptAndTruncatedPagesFailClosed(t *testing.T) {
	fixture := newIndexFixture(t)
	ctx := context.Background()
	root := mustEmpty(t, fixture.tree)
	root = putValue(t, fixture.tree, root, keyWithUint64(9), []byte("record"))
	object := fixture.store.objects[root.Locator]

	delete(fixture.store.objects, root.Locator)
	if _, err := fixture.tree.Get(ctx, root, keyWithUint64(9)); err == nil {
		t.Fatal("missing page was accepted")
	}
	fixture.store.objects[root.Locator] = object

	fixture.store.objects[root.Locator] = storedPage{identity: object.identity, bytes: object.bytes[:len(object.bytes)-1]}
	if _, err := fixture.tree.Get(ctx, root, keyWithUint64(9)); err == nil {
		t.Fatal("truncated encrypted page was accepted")
	}
	fixture.store.objects[root.Locator] = object

	corrupt := bytes.Clone(object.bytes)
	corrupt[len(corrupt)/2] ^= 0x80
	fixture.store.objects[root.Locator] = storedPage{identity: object.identity, bytes: corrupt}
	if _, err := fixture.tree.Get(ctx, root, keyWithUint64(9)); err == nil {
		t.Fatal("corrupt encrypted page was accepted")
	}
	fixture.store.objects[root.Locator] = object

	var id [16]byte
	id[0], id[15] = 0xfd, 1
	identity := vaultpage.Identity{Role: vaultpage.RoleIndex, LogicalID: id, Generation: 1}
	stored, digest, err := fixture.codec.Seal(identity, []byte(`{"version":1`))
	if err != nil {
		t.Fatal(err)
	}
	locator, err := fixture.store.Put(ctx, identity, stored, digest)
	if err != nil {
		t.Fatal(err)
	}
	truncatedJSON := vaultpage.Reference{Locator: locator, Identity: identity, Digest: digest}
	if _, err := fixture.tree.Get(ctx, truncatedJSON, keyWithUint64(0)); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("truncated authenticated JSON error = %v", err)
	}
}

func TestContextCancellationAndValueLimit(t *testing.T) {
	fixture := newIndexFixture(t)
	root := mustEmpty(t, fixture.tree)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	puts, gets := fixture.store.puts, fixture.store.gets
	if _, err := fixture.tree.Put(ctx, root, keyWithUint64(1), []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled put error = %v", err)
	}
	if _, err := fixture.tree.Get(ctx, root, keyWithUint64(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled get error = %v", err)
	}
	if fixture.store.puts != puts || fixture.store.gets != gets {
		t.Fatal("canceled operation reached the store")
	}
	if _, err := fixture.tree.Put(context.Background(), root, keyWithUint64(1), make([]byte, MaxValueSize+1)); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("oversized value error = %v", err)
	}
	if fixture.store.puts != puts {
		t.Fatal("oversized value reached the store")
	}
}

func TestReferencesAndEverySuccessfulPageIDAreFresh(t *testing.T) {
	fixture := newIndexFixture(t)
	root := mustEmpty(t, fixture.tree)
	refs := []vaultpage.Reference{root}
	for i := uint64(0); i < 80; i++ {
		root = putValue(t, fixture.tree, root, keyWithUint64(i), []byte("v"))
		refs = append(refs, root)
	}
	ids := make(map[[16]byte]struct{})
	locators := make(map[string]struct{})
	for _, object := range fixture.store.objects {
		if _, found := ids[object.identity.LogicalID]; found {
			t.Fatalf("reused identity %x", object.identity.LogicalID)
		}
		ids[object.identity.LogicalID] = struct{}{}
	}
	for _, ref := range refs {
		if err := ref.Validate(vaultpage.RoleIndex); err != nil {
			t.Fatalf("invalid root reference: %v", err)
		}
		if _, found := locators[ref.Locator]; found {
			t.Fatalf("root locator reused: %q", ref.Locator)
		}
		locators[ref.Locator] = struct{}{}
	}
	if len(ids) != len(fixture.store.objects) {
		t.Fatalf("successful stored page IDs = %d, objects = %d", len(ids), len(fixture.store.objects))
	}
}

func TestWalkOrderIsStable(t *testing.T) {
	fixture := newIndexFixture(t)
	root := mustEmpty(t, fixture.tree)
	for _, value := range []uint64{99, 1, 50, 3, 65, 8} {
		root = putValue(t, fixture.tree, root, keyWithUint64(value), []byte{byte(value)})
	}
	var got []Key
	if err := fixture.tree.Walk(context.Background(), root, func(key Key, _ []byte) error {
		got = append(got, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := append([]Key(nil), got...)
	sort.Slice(want, func(i, j int) bool { return bytes.Compare(want[i][:], want[j][:]) < 0 })
	if len(got) != len(want) {
		t.Fatalf("walk count = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("walk key %d is out of order", i)
		}
	}
}
