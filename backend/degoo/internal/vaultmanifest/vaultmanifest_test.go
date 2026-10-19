package vaultmanifest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/rclone/rclone/backend/degoo/internal/vaultpage"
)

type memoryStore struct {
	pages    map[string][]byte
	puts     int
	gets     int
	failAt   int
	nextName int
	prefix   string
}

type discardStore struct {
	puts int
}

// discardStore isolates builder memory; the test never reads from it.
func (s *discardStore) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("discard store does not support reads")
}

func (s *discardStore) Put(_ context.Context, _ vaultpage.Identity, _ []byte, _ [32]byte) (string, error) {
	s.puts++
	return fmt.Sprintf("discard-%d", s.puts), nil
}

func newMemoryStore() *memoryStore {
	return &memoryStore{pages: make(map[string][]byte)}
}

func (s *memoryStore) Get(_ context.Context, locator string) ([]byte, error) {
	s.gets++
	value, ok := s.pages[locator]
	if !ok {
		return nil, fmt.Errorf("missing test page %q", locator)
	}
	return append([]byte(nil), value...), nil
}

func (s *memoryStore) Put(_ context.Context, _ vaultpage.Identity, stored []byte, digest [32]byte) (string, error) {
	s.puts++
	if s.failAt != 0 && s.puts == s.failAt {
		return "", errors.New("injected create failure")
	}
	if digest != sha256.Sum256(stored) {
		return "", errors.New("test store received incorrect digest")
	}
	s.nextName++
	name := fmt.Sprintf("%s%04d", s.prefix, s.nextName)
	s.pages[name] = append([]byte(nil), stored...)
	return name, nil
}

func newTestTree(t *testing.T) (*Tree, *memoryStore) {
	t.Helper()
	var salt [32]byte
	var vaultID [16]byte
	for i := range salt {
		salt[i] = byte(i + 1)
	}
	for i := range vaultID {
		vaultID[i] = byte(i + 19)
	}
	codec, err := vaultpage.New("manifest-test-password", salt, vaultID)
	if err != nil {
		t.Fatalf("create page codec: %v", err)
	}
	store := newMemoryStore()
	repository, err := vaultpage.NewRepository(codec, store)
	if err != nil {
		t.Fatalf("create page repository: %v", err)
	}
	tree, err := New(repository)
	if err != nil {
		t.Fatalf("create manifest tree: %v", err)
	}
	return tree, store
}

func testFileID() [16]byte {
	var id [16]byte
	for i := range id {
		id[i] = byte(i + 1)
	}
	return id
}

func testChunk(offset int64) Chunk {
	var digest, header [32]byte
	digest[0] = 1
	header[0] = 2
	return Chunk{Offset: offset, Size: 1, StoredSize: 65, ID: fmt.Sprintf("data-%d", offset), Digest: digest, CryptHeader: header}
}

func sequentialChunks(count int) NextChunk {
	next := 0
	return func(context.Context) (Chunk, error) {
		if next == count {
			return Chunk{}, io.EOF
		}
		chunk := testChunk(int64(next))
		next++
		return chunk, nil
	}
}

func TestBuildStreamsManyChunksAndLookupUsesOnePath(t *testing.T) {
	tree, store := newTestTree(t)
	const count = 4097
	manifest, err := tree.Build(context.Background(), testFileID(), sequentialChunks(count))
	if err != nil {
		t.Fatalf("Build returned an error: %v", err)
	}
	if manifest.Size != count {
		t.Fatalf("manifest size = %d, want %d", manifest.Size, count)
	}
	if store.puts != 68 { // 65 leaves, two level-one branches and one root.
		t.Fatalf("staged pages = %d, want 68", store.puts)
	}
	for _, offset := range []int64{0, 63, 64, 4095, 4096} {
		store.gets = 0
		chunk, err := tree.Lookup(context.Background(), manifest, offset)
		if err != nil {
			t.Fatalf("Lookup(%d) returned an error: %v", offset, err)
		}
		if chunk.Offset != offset || chunk.ID != fmt.Sprintf("data-%d", offset) {
			t.Fatalf("Lookup(%d) returned chunk %+v", offset, chunk)
		}
		if store.gets != 3 {
			t.Fatalf("Lookup(%d) read %d pages, want one three-page path", offset, store.gets)
		}
	}
	store.gets = 0
	if _, err := tree.Lookup(context.Background(), manifest, count); !errors.Is(err, io.EOF) {
		t.Fatalf("lookup at end error = %v, want EOF", err)
	}
	if store.gets != 0 {
		t.Fatalf("out-of-range lookup read %d pages", store.gets)
	}
}

func TestBuilderFitsWorstCaseBoundedPagePayloads(t *testing.T) {
	tree, store := newTestTree(t)
	store.prefix = strings.Repeat("\x01", vaultpage.MaxLocatorSize-4)
	const count = MaxChunksPerPage * MaxChunksPerPage
	index := 0
	next := func(context.Context) (Chunk, error) {
		if index == count {
			return Chunk{}, io.EOF
		}
		chunk := testChunk(int64(index) * MaxChunkSize)
		chunk.Size = MaxChunkSize
		chunk.StoredSize = MaxStoredChunkSize
		chunk.ID = strings.Repeat("\x01", vaultpage.MaxLocatorSize)
		index++
		return chunk, nil
	}
	manifest, err := tree.Build(context.Background(), testFileID(), next)
	if err != nil {
		t.Fatalf("Build with maximum bounded descriptors: %v", err)
	}
	if manifest.Size != int64(count)*MaxChunkSize {
		t.Fatalf("manifest size = %d, want %d", manifest.Size, int64(count)*MaxChunkSize)
	}
	if store.puts != count/MaxChunksPerPage+1 {
		t.Fatalf("staged pages = %d, want %d leaves plus root", store.puts, count/MaxChunksPerPage+1)
	}
}

func TestBuildRetainsBoundedMemoryForThreeBranchLevels(t *testing.T) {
	const count = MaxChunksPerPage*MaxChunksPerPage*MaxChunksPerPage + 1
	var salt [32]byte
	var vaultID [16]byte
	for i := range salt {
		salt[i] = byte(i + 1)
	}
	for i := range vaultID {
		vaultID[i] = byte(i + 19)
	}
	codec, err := vaultpage.New("manifest-memory-test-password", salt, vaultID)
	if err != nil {
		t.Fatal(err)
	}
	store := &discardStore{}
	repository, err := vaultpage.NewRepository(codec, store)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := New(repository)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	index := 0
	next := func(context.Context) (Chunk, error) {
		if index == count {
			return Chunk{}, io.EOF
		}
		chunk := testChunk(int64(index))
		index++
		return chunk, nil
	}
	manifest, err := tree.Build(context.Background(), testFileID(), next)
	if err != nil {
		t.Fatalf("Build streamed tree: %v", err)
	}
	if manifest.Size != count || store.puts != 4165 {
		t.Fatalf("manifest size/pages = %d/%d, want %d/4165", manifest.Size, store.puts, count)
	}
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if retained > 12<<20 {
		t.Fatalf("Build retained %d bytes after a %d-chunk stream, want under 12 MiB", retained, count)
	}
}

func TestBuildEmptyFileAndBoundaries(t *testing.T) {
	tree, store := newTestTree(t)
	empty, err := tree.Build(context.Background(), testFileID(), sequentialChunks(0))
	if err != nil {
		t.Fatalf("Build empty file: %v", err)
	}
	if empty.Size != 0 || store.puts != 1 {
		t.Fatalf("empty manifest size/puts = %d/%d", empty.Size, store.puts)
	}
	payload, err := tree.repo.Get(context.Background(), empty.Root)
	if err != nil {
		t.Fatalf("read empty leaf: %v", err)
	}
	node, err := decodeNode(payload)
	if err != nil || node.level != 0 || node.start != 0 || node.size != 0 || len(node.chunks) != 0 {
		t.Fatalf("empty root page = %+v, %v", node, err)
	}
	if _, err := tree.Lookup(context.Background(), empty, 0); !errors.Is(err, io.EOF) {
		t.Fatalf("empty lookup error = %v, want EOF", err)
	}

	one, err := tree.Build(context.Background(), testFileID(), sequentialChunks(1))
	if err != nil {
		t.Fatalf("Build one-chunk file: %v", err)
	}
	chunk, err := tree.Lookup(context.Background(), one, 0)
	if err != nil || chunk.Offset != 0 {
		t.Fatalf("lookup first byte = %+v, %v", chunk, err)
	}
	if _, err := tree.Lookup(context.Background(), one, 1); !errors.Is(err, io.EOF) {
		t.Fatalf("lookup at file end error = %v, want EOF", err)
	}
}

func TestLookupAllowsExactlyEightBranchLevels(t *testing.T) {
	tree, store := newTestTree(t)
	fileID := testFileID()
	leaf := nodeJSON{Version: manifestVersion, FileID: fixed16(fileID), Start: 0, Size: 1, Chunks: &[]chunkJSON{toChunkJSON(testChunk(0))}}
	ref, err := putNode(t, tree, leaf, 1)
	if err != nil {
		t.Fatal(err)
	}
	for level := 1; level <= MaxBranchLevels; level++ {
		child := childJSON{Start: 0, Size: 1, Reference: toReferenceJSON(ref)}
		children := []childJSON{child}
		node := nodeJSON{Version: manifestVersion, FileID: fixed16(fileID), Start: 0, Size: 1, Level: uint8(level), Children: &children}
		ref, err = putNode(t, tree, node, uint64(level+1))
		if err != nil {
			t.Fatal(err)
		}
	}
	manifest := Manifest{FileID: fileID, Size: 1, Root: ref}
	store.gets = 0
	if _, err := tree.Lookup(context.Background(), manifest, 0); err != nil {
		t.Fatalf("lookup through maximum-depth tree: %v", err)
	}
	if store.gets != MaxBranchLevels+1 {
		t.Fatalf("page reads = %d, want %d", store.gets, MaxBranchLevels+1)
	}
}

func TestLookupRejectsMalformedAndOverDepthPages(t *testing.T) {
	tests := []struct {
		name    string
		payload func([16]byte) []byte
		want    error
	}{
		{
			name: "unknown field",
			payload: func(fileID [16]byte) []byte {
				node := nodeJSON{Version: manifestVersion, FileID: fixed16(fileID), Start: 0, Size: 1, Chunks: &[]chunkJSON{toChunkJSON(testChunk(0))}}
				return appendJSONField(t, node, `"future":true`)
			},
			want: ErrInvalidNode,
		},
		{
			name: "trailing object",
			payload: func(fileID [16]byte) []byte {
				node := nodeJSON{Version: manifestVersion, FileID: fixed16(fileID), Start: 0, Size: 1, Chunks: &[]chunkJSON{toChunkJSON(testChunk(0))}}
				payload, _ := json.Marshal(node)
				return append(payload, []byte(` {}`)...)
			},
			want: ErrInvalidNode,
		},
		{
			name: "wrong fixed array length",
			payload: func(fileID [16]byte) []byte {
				file, _ := json.Marshal(fileID)
				return []byte(fmt.Sprintf(`{"version":1,"file_id":%s,"start":0,"size":1,"level":0,"chunks":[{"offset":0,"size":1,"stored_size":65,"id":"x","digest":[1],"crypt_header":[2]}]}`, file))
			},
			want: ErrInvalidNode,
		},
		{
			name: "range overflow",
			payload: func(fileID [16]byte) []byte {
				file, _ := json.Marshal(fileID)
				return []byte(fmt.Sprintf(`{"version":1,"file_id":%s,"start":9223372036854775807,"size":1,"level":0,"chunks":[]}`, file))
			},
			want: ErrInvalidNode,
		},
		{
			name: "branch gap",
			payload: func(fileID [16]byte) []byte {
				ref := referenceFixture()
				children := []childJSON{
					{Start: 0, Size: 1, Reference: toReferenceJSON(ref)},
					{Start: 2, Size: 1, Reference: toReferenceJSON(ref)},
				}
				node := nodeJSON{Version: manifestVersion, FileID: fixed16(fileID), Start: 0, Size: 3, Level: 1, Children: &children}
				payload, _ := json.Marshal(node)
				return payload
			},
			want: ErrInvalidNode,
		},
		{
			name: "over depth",
			payload: func(fileID [16]byte) []byte {
				ref := referenceFixture()
				children := []childJSON{{Start: 0, Size: 1, Reference: toReferenceJSON(ref)}}
				node := nodeJSON{Version: manifestVersion, FileID: fixed16(fileID), Start: 0, Size: 1, Level: MaxBranchLevels + 1, Children: &children}
				payload, _ := json.Marshal(node)
				return payload
			},
			want: ErrDepthLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree, store := newTestTree(t)
			fileID := testFileID()
			ref, err := putRawPage(t, tree, test.payload(fileID), 99)
			if err != nil {
				t.Fatal(err)
			}
			manifest := Manifest{FileID: fileID, Size: 1, Root: ref}
			if _, err := tree.Lookup(context.Background(), manifest, 0); !errors.Is(err, test.want) {
				t.Fatalf("Lookup error = %v, want %v", err, test.want)
			}
			if test.name == "over depth" && store.gets != 1 {
				t.Fatalf("over-depth page read %d pages, want 1", store.gets)
			}
		})
	}
}

func TestLookupRejectsWrongFileAndReferenceIntegrityFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest, *memoryStore)
		want   error
	}{
		{
			name: "wrong file ID",
			mutate: func(manifest *Manifest, _ *memoryStore) {
				manifest.FileID[0] ^= 1
			},
			want: ErrInvalidNode,
		},
		{
			name: "reference digest",
			mutate: func(manifest *Manifest, _ *memoryStore) {
				manifest.Root.Digest[0] ^= 1
			},
			want: vaultpage.ErrDigestMismatch,
		},
		{
			name: "expected page identity",
			mutate: func(manifest *Manifest, _ *memoryStore) {
				manifest.Root.Identity.LogicalID[0] ^= 1
			},
			want: vaultpage.ErrIdentityMismatch,
		},
		{
			name: "corrupt stored bytes",
			mutate: func(manifest *Manifest, store *memoryStore) {
				store.pages[manifest.Root.Locator][0] ^= 1
			},
			want: vaultpage.ErrDigestMismatch,
		},
		{
			name: "missing page",
			mutate: func(manifest *Manifest, store *memoryStore) {
				delete(store.pages, manifest.Root.Locator)
			},
			want: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree, store := newTestTree(t)
			manifest, err := tree.Build(context.Background(), testFileID(), sequentialChunks(1))
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&manifest, store)
			_, err = tree.Lookup(context.Background(), manifest, 0)
			if err == nil {
				t.Fatal("Lookup unexpectedly succeeded")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("Lookup error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestLookupRejectsChildRangeLevelAndFileMismatch(t *testing.T) {
	tests := []struct {
		name      string
		childNode nodeJSON
		rootLevel uint8
	}{
		{
			name:      "child range",
			childNode: nodeJSON{Version: manifestVersion, FileID: fixed16(testFileID()), Start: 1, Size: 1, Chunks: &[]chunkJSON{toChunkJSON(testChunk(1))}},
			rootLevel: 1,
		},
		{
			name:      "child level",
			childNode: nodeJSON{Version: manifestVersion, FileID: fixed16(testFileID()), Start: 0, Size: 1, Chunks: &[]chunkJSON{toChunkJSON(testChunk(0))}},
			rootLevel: 2,
		},
		{
			name:      "child file ID",
			childNode: nodeJSON{Version: manifestVersion, FileID: fixed16([16]byte{99}), Start: 0, Size: 1, Chunks: &[]chunkJSON{toChunkJSON(testChunk(0))}},
			rootLevel: 1,
		},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree, store := newTestTree(t)
			fileID := testFileID()
			childRef, err := putNode(t, tree, test.childNode, uint64(10+i))
			if err != nil {
				t.Fatal(err)
			}
			children := []childJSON{{Start: 0, Size: 1, Reference: toReferenceJSON(childRef)}}
			root := nodeJSON{Version: manifestVersion, FileID: fixed16(fileID), Start: 0, Size: 1, Level: test.rootLevel, Children: &children}
			rootRef, err := putNode(t, tree, root, uint64(20+i))
			if err != nil {
				t.Fatal(err)
			}
			_, err = tree.Lookup(context.Background(), Manifest{FileID: fileID, Size: 1, Root: rootRef}, 0)
			if !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("Lookup error = %v, want ErrInvalidNode", err)
			}
			if store.gets != 2 {
				t.Fatalf("page reads = %d, want root and selected child", store.gets)
			}
		})
	}
}

func TestBuildRejectsInvalidStreamsAndDoesNotRetryWrites(t *testing.T) {
	tests := []struct {
		name string
		next NextChunk
	}{
		{
			name: "gap",
			next: func(context.Context) (Chunk, error) { return testChunk(1), nil },
		},
		{
			name: "oversized logical chunk",
			next: func(context.Context) (Chunk, error) {
				chunk := testChunk(0)
				chunk.Size = MaxChunkSize + 1
				return chunk, nil
			},
		},
		{
			name: "stored size cap",
			next: func(context.Context) (Chunk, error) {
				chunk := testChunk(0)
				chunk.StoredSize = MaxStoredChunkSize + 1
				return chunk, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree, store := newTestTree(t)
			if _, err := tree.Build(context.Background(), testFileID(), test.next); !errors.Is(err, ErrInvalidChunk) {
				t.Fatalf("Build error = %v, want ErrInvalidChunk", err)
			}
			if store.puts != 0 {
				t.Fatalf("invalid stream staged %d pages", store.puts)
			}
		})
	}

	tree, store := newTestTree(t)
	store.failAt = 1
	if _, err := tree.Build(context.Background(), testFileID(), sequentialChunks(1)); err == nil {
		t.Fatal("Build unexpectedly succeeded after create failure")
	}
	if store.puts != 1 {
		t.Fatalf("create attempts = %d, want one attempt", store.puts)
	}
}

func TestManifestAndChunkBounds(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, vaultpage.ErrInvalidConfig) {
		t.Fatalf("New(nil) error = %v", err)
	}
	chunk := testChunk(0)
	if err := validateChunk(chunk); err != nil {
		t.Fatalf("valid chunk rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Chunk){
		"empty locator": func(c *Chunk) { c.ID = "" },
		"large locator": func(c *Chunk) { c.ID = strings.Repeat("x", vaultpage.MaxLocatorSize+1) },
		"NUL locator":   func(c *Chunk) { c.ID = "x\x00y" },
		"zero digest":   func(c *Chunk) { c.Digest = [32]byte{} },
		"zero header":   func(c *Chunk) { c.CryptHeader = [32]byte{} },
		"stored not encrypted": func(c *Chunk) {
			c.StoredSize = c.Size
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := chunk
			mutate(&invalid)
			if err := validateChunk(invalid); !errors.Is(err, ErrInvalidChunk) {
				t.Fatalf("validateChunk error = %v", err)
			}
		})
	}
}

func putNode(t *testing.T, tree *Tree, node nodeJSON, generation uint64) (vaultpage.Reference, error) {
	t.Helper()
	payload, err := json.Marshal(node)
	if err != nil {
		return vaultpage.Reference{}, err
	}
	return putRawPage(t, tree, payload, generation)
}

func putRawPage(t *testing.T, tree *Tree, payload []byte, generation uint64) (vaultpage.Reference, error) {
	t.Helper()
	var logicalID [16]byte
	logicalID[0] = byte(generation)
	logicalID[15] = 1
	return tree.repo.Put(context.Background(), vaultpage.Identity{Role: vaultpage.RoleManifest, LogicalID: logicalID, Generation: generation}, payload)
}

func referenceFixture() vaultpage.Reference {
	return vaultpage.Reference{
		Locator:  "fixture-page",
		Identity: vaultpage.Identity{Role: vaultpage.RoleManifest, LogicalID: [16]byte{1}, Generation: 1},
		Digest:   [32]byte{1},
	}
}

func appendJSONField(t *testing.T, value any, field string) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return append(payload[:len(payload)-1], []byte(","+field+"}")...)
}
