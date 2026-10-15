// Package vaultmanifest builds and reads bounded encrypted chunk manifests.
//
// The package is an inactive format component. It does not perform file-data
// I/O, publish roots, or provide provider durability and transaction ownership.
package vaultmanifest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/rclone/rclone/backend/degoo/internal/vaultpage"
)

const (
	// MaxChunksPerPage bounds leaf descriptors and branch references.
	MaxChunksPerPage = 64
	// MaxBranchLevels bounds the number of branch pages from root to leaf.
	MaxBranchLevels = 8
	// MaxChunkSize bounds the logical size of one data chunk.
	MaxChunkSize int64 = 1 << 30
	// MaxStoredChunkSize includes crypt stream framing and the encrypted data identity envelope.
	MaxStoredChunkSize int64 = MaxChunkSize + (1 << 20)

	manifestVersion = 1
)

var (
	// ErrInvalidManifest reports an invalid trusted manifest descriptor.
	ErrInvalidManifest = errors.New("vault manifest: invalid manifest")
	// ErrInvalidChunk reports an invalid chunk descriptor.
	ErrInvalidChunk = errors.New("vault manifest: invalid chunk descriptor")
	// ErrInvalidNode reports malformed or inconsistent manifest page data.
	ErrInvalidNode = errors.New("vault manifest: invalid manifest page")
	// ErrDepthLimit reports a tree that exceeds the supported branch depth.
	ErrDepthLimit = errors.New("vault manifest: maximum tree depth exceeded")
	// ErrPayloadTooLarge reports a node that exceeds the page codec payload cap.
	ErrPayloadTooLarge = errors.New("vault manifest: page payload exceeds limit")
)

// Chunk describes one immutable encrypted data object. The manifest library
// validates these fields but does not open the object or verify its contents.
type Chunk struct {
	// Offset is the chunk's absolute logical file offset.
	Offset int64 `json:"offset"`
	// Size is the positive plaintext byte length.
	Size int64 `json:"size"`
	// StoredSize includes the encrypted identity envelope and crypt framing.
	StoredSize int64 `json:"stored_size"`
	// ID is the bounded opaque immutable-object locator.
	ID string `json:"id"`
	// Digest commits to the complete stored ciphertext.
	Digest [32]byte `json:"digest"`
	// CryptHeader binds range reads to the expected crypt stream nonce.
	CryptHeader [32]byte `json:"crypt_header"`
}

// NextChunk supplies the next contiguous chunk descriptor, or io.EOF.
type NextChunk func(ctx context.Context) (Chunk, error)

// Manifest is a trusted file record pointing at the root of an encrypted tree.
type Manifest struct {
	// FileID binds the tree to one stable logical file identity.
	FileID [16]byte `json:"file_id"`
	// Size is the complete logical file length, including zero for empty files.
	Size int64 `json:"size"`
	// Root identifies the authenticated manifest-tree root page.
	Root vaultpage.Reference `json:"root"`
}

// Tree builds and reads manifest pages through a verified page repository.
type Tree struct {
	repo *vaultpage.Repository
}

// New binds a manifest tree to an initialized page repository.
func New(repo *vaultpage.Repository) (*Tree, error) {
	if repo == nil || repo.MaxPayload() <= 0 {
		return nil, vaultpage.ErrInvalidConfig
	}
	return &Tree{repo: repo}, nil
}

// Build writes a balanced manifest from a stream of contiguous chunk descriptors.
// Staged pages are left to the transaction owner if any operation fails.
func (t *Tree) Build(ctx context.Context, fileID [16]byte, next NextChunk) (Manifest, error) {
	if t == nil || t.repo == nil || allZero(fileID[:]) || next == nil {
		return Manifest{}, ErrInvalidManifest
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}

	pending := make([][]nodeRecord, MaxBranchLevels+1)
	var push func(nodeRecord) error
	push = func(record nodeRecord) error {
		if record.level < 0 || record.level > MaxBranchLevels {
			return ErrDepthLimit
		}
		pending[record.level] = append(pending[record.level], record)
		if len(pending[record.level]) < MaxChunksPerPage {
			return nil
		}
		if record.level == MaxBranchLevels {
			return ErrDepthLimit
		}
		group := pending[record.level]
		pending[record.level] = nil
		parent, err := t.writeBranch(ctx, fileID, record.level+1, group)
		if err != nil {
			return err
		}
		return push(parent)
	}

	leaf := make([]Chunk, 0, MaxChunksPerPage)
	var size int64
	flushLeaf := func() error {
		start := size
		if len(leaf) != 0 {
			start = leaf[0].Offset
		}
		leafSize := size - start
		record, err := t.writeLeaf(ctx, fileID, start, leafSize, leaf)
		if err != nil {
			return err
		}
		leaf = make([]Chunk, 0, MaxChunksPerPage)
		return push(record)
	}

	empty := true
	for {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		chunk, err := next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("vault manifest: read next chunk: %w", err)
		}
		if err := validateChunk(chunk); err != nil {
			return Manifest{}, err
		}
		if chunk.Offset != size {
			return Manifest{}, fmt.Errorf("%w: chunk offset %d, expected %d", ErrInvalidChunk, chunk.Offset, size)
		}
		end, ok := checkedEnd(size, chunk.Size)
		if !ok {
			return Manifest{}, fmt.Errorf("%w: logical size overflows int64", ErrInvalidChunk)
		}
		chunk.ID = strings.Clone(chunk.ID)
		leaf = append(leaf, chunk)
		size = end
		empty = false
		if len(leaf) == MaxChunksPerPage {
			if err := flushLeaf(); err != nil {
				return Manifest{}, err
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if empty || len(leaf) != 0 {
		if err := flushLeaf(); err != nil {
			return Manifest{}, err
		}
	}

	var root nodeRecord
	for level := 0; level <= MaxBranchLevels; level++ {
		if len(pending[level]) == 0 {
			continue
		}
		higher := false
		for nextLevel := level + 1; nextLevel <= MaxBranchLevels; nextLevel++ {
			if len(pending[nextLevel]) != 0 {
				higher = true
				break
			}
		}
		if len(pending[level]) == 1 && !higher {
			root = pending[level][0]
			return Manifest{FileID: fileID, Size: size, Root: root.ref}, nil
		}
		if level == MaxBranchLevels {
			return Manifest{}, ErrDepthLimit
		}
		group := pending[level]
		pending[level] = nil
		parent, err := t.writeBranch(ctx, fileID, level+1, group)
		if err != nil {
			return Manifest{}, err
		}
		if err := push(parent); err != nil {
			return Manifest{}, err
		}
	}
	return Manifest{}, fmt.Errorf("%w: builder produced no root", ErrInvalidManifest)
}

// Lookup returns the chunk covering offset after reading only its root-to-leaf path.
func (t *Tree) Lookup(ctx context.Context, manifest Manifest, offset int64) (Chunk, error) {
	if t == nil || t.repo == nil || validateManifest(manifest) != nil || offset < 0 {
		return Chunk{}, ErrInvalidManifest
	}
	if err := ctx.Err(); err != nil {
		return Chunk{}, err
	}
	if offset >= manifest.Size {
		return Chunk{}, io.EOF
	}

	ref := manifest.Root
	expectedLevel := -1
	expectedStart := int64(0)
	expectedSize := int64(0)
	for reads := 0; reads <= MaxBranchLevels; reads++ {
		if err := ctx.Err(); err != nil {
			return Chunk{}, err
		}
		payload, err := t.repo.Get(ctx, ref)
		if err != nil {
			return Chunk{}, err
		}
		n, err := decodeNode(payload)
		if err != nil {
			return Chunk{}, err
		}
		if n.fileID != manifest.FileID {
			return Chunk{}, fmt.Errorf("%w: wrong file ID", ErrInvalidNode)
		}
		if expectedLevel >= 0 && (int(n.level) != expectedLevel || n.start != expectedStart || n.size != expectedSize) {
			return Chunk{}, fmt.Errorf("%w: child identity level or range mismatch", ErrInvalidNode)
		}
		if n.level > MaxBranchLevels || n.start < 0 || n.size < 0 {
			return Chunk{}, ErrInvalidNode
		}
		end, ok := checkedEnd(n.start, n.size)
		if !ok {
			return Chunk{}, fmt.Errorf("%w: range overflows int64", ErrInvalidNode)
		}
		if reads == 0 {
			if n.start != 0 || n.size != manifest.Size {
				return Chunk{}, fmt.Errorf("%w: root range does not match file record", ErrInvalidNode)
			}
		} else if offset < n.start || offset >= end {
			return Chunk{}, fmt.Errorf("%w: child range does not cover requested offset", ErrInvalidNode)
		}

		if n.level == 0 {
			chunk, err := lookupLeaf(n, offset)
			if err != nil {
				return Chunk{}, err
			}
			return chunk, nil
		}
		if reads == MaxBranchLevels {
			return Chunk{}, ErrDepthLimit
		}
		child, err := selectChild(n, offset, end)
		if err != nil {
			return Chunk{}, err
		}
		if child.ref.Identity.Generation == 0 || child.ref.Identity.Role != vaultpage.RoleManifest {
			return Chunk{}, ErrInvalidNode
		}
		ref = child.ref
		expectedLevel = int(n.level) - 1
		expectedStart = child.start
		expectedSize = child.size
	}
	return Chunk{}, ErrDepthLimit
}

type nodeRecord struct {
	start int64
	size  int64
	level int
	ref   vaultpage.Reference
}

func (t *Tree) writeLeaf(ctx context.Context, fileID [16]byte, start, size int64, chunks []Chunk) (nodeRecord, error) {
	encodedChunks := make([]chunkJSON, len(chunks))
	n := nodeJSON{Version: manifestVersion, FileID: fixed16(fileID), Start: start, Size: size, Level: 0, Chunks: &encodedChunks}
	for i, chunk := range chunks {
		encodedChunks[i] = toChunkJSON(chunk)
	}
	return t.writeNode(ctx, n)
}

func (t *Tree) writeBranch(ctx context.Context, fileID [16]byte, level int, children []nodeRecord) (nodeRecord, error) {
	if level < 1 || level > MaxBranchLevels || len(children) == 0 || len(children) > MaxChunksPerPage {
		return nodeRecord{}, ErrDepthLimit
	}
	start := children[0].start
	end := start
	encodedChildren := make([]childJSON, len(children))
	n := nodeJSON{Version: manifestVersion, FileID: fixed16(fileID), Start: start, Level: uint8(level), Children: &encodedChildren}
	for i, child := range children {
		if child.level != level-1 || child.start != end || child.size <= 0 || child.ref.Validate(vaultpage.RoleManifest) != nil {
			return nodeRecord{}, fmt.Errorf("%w: invalid builder child", ErrInvalidNode)
		}
		childEnd, ok := checkedEnd(child.start, child.size)
		if !ok {
			return nodeRecord{}, ErrInvalidNode
		}
		end = childEnd
		encodedChildren[i] = childJSON{Start: child.start, Size: child.size, Reference: toReferenceJSON(child.ref)}
	}
	n.Size = end - start
	return t.writeNode(ctx, n)
}

func (t *Tree) writeNode(ctx context.Context, n nodeJSON) (nodeRecord, error) {
	if err := ctx.Err(); err != nil {
		return nodeRecord{}, err
	}
	payload, err := json.Marshal(n)
	if err != nil {
		return nodeRecord{}, fmt.Errorf("vault manifest: encode page: %w", err)
	}
	if len(payload) > t.repo.MaxPayload() {
		return nodeRecord{}, ErrPayloadTooLarge
	}
	if err := validateNodeData(fromNodeJSON(n)); err != nil {
		return nodeRecord{}, err
	}
	var logicalID [16]byte
	if _, err := rand.Read(logicalID[:]); err != nil {
		return nodeRecord{}, fmt.Errorf("vault manifest: generate page identity: %w", err)
	}
	if allZero(logicalID[:]) {
		return nodeRecord{}, fmt.Errorf("vault manifest: random page identity is zero")
	}
	identity := vaultpage.Identity{Role: vaultpage.RoleManifest, LogicalID: logicalID, Generation: 1}
	ref, err := t.repo.Put(ctx, identity, payload)
	if err != nil {
		return nodeRecord{}, err
	}
	data := fromNodeJSON(n)
	return nodeRecord{start: data.start, size: data.size, level: int(data.level), ref: ref}, nil
}

type nodeData struct {
	fileID   [16]byte
	start    int64
	size     int64
	level    uint8
	chunks   []Chunk
	children []childData
}

type childData struct {
	start int64
	size  int64
	ref   vaultpage.Reference
}

func validateManifest(manifest Manifest) error {
	if allZero(manifest.FileID[:]) || manifest.Size < 0 || manifest.Root.Validate(vaultpage.RoleManifest) != nil {
		return ErrInvalidManifest
	}
	return nil
}

func validateChunk(chunk Chunk) error {
	if chunk.Offset < 0 || chunk.Size <= 0 || chunk.Size > MaxChunkSize || chunk.StoredSize <= chunk.Size || chunk.StoredSize > MaxStoredChunkSize || !validLocator(chunk.ID) || allZero(chunk.Digest[:]) || allZero(chunk.CryptHeader[:]) {
		return ErrInvalidChunk
	}
	if _, ok := checkedEnd(chunk.Offset, chunk.Size); !ok {
		return ErrInvalidChunk
	}
	return nil
}

func validateNodeData(n nodeData) error {
	if n.level > MaxBranchLevels {
		return ErrDepthLimit
	}
	if allZero(n.fileID[:]) || n.start < 0 || n.size < 0 {
		return ErrInvalidNode
	}
	end, ok := checkedEnd(n.start, n.size)
	if !ok {
		return fmt.Errorf("%w: range overflows int64", ErrInvalidNode)
	}
	if n.level == 0 {
		if len(n.chunks) > MaxChunksPerPage || len(n.children) != 0 {
			return ErrInvalidNode
		}
		if len(n.chunks) == 0 {
			if n.start != 0 || n.size != 0 {
				return fmt.Errorf("%w: empty leaf has nonempty range", ErrInvalidNode)
			}
			return nil
		}
		if n.size == 0 {
			return ErrInvalidNode
		}
		position := n.start
		for _, chunk := range n.chunks {
			if err := validateChunk(chunk); err != nil || chunk.Offset != position {
				return fmt.Errorf("%w: invalid or noncontiguous leaf chunk", ErrInvalidNode)
			}
			position, ok = checkedEnd(position, chunk.Size)
			if !ok {
				return fmt.Errorf("%w: chunk range overflows", ErrInvalidNode)
			}
		}
		if position != end {
			return fmt.Errorf("%w: leaf range does not match chunks", ErrInvalidNode)
		}
		return nil
	}
	if n.size <= 0 || len(n.chunks) != 0 || len(n.children) == 0 || len(n.children) > MaxChunksPerPage {
		return ErrInvalidNode
	}
	position := n.start
	for _, child := range n.children {
		if child.start != position || child.size <= 0 || child.ref.Validate(vaultpage.RoleManifest) != nil {
			return fmt.Errorf("%w: invalid or noncontiguous child range", ErrInvalidNode)
		}
		position, ok = checkedEnd(position, child.size)
		if !ok {
			return fmt.Errorf("%w: child range overflows", ErrInvalidNode)
		}
	}
	if position != end {
		return fmt.Errorf("%w: branch range does not match children", ErrInvalidNode)
	}
	return nil
}

func lookupLeaf(n nodeData, offset int64) (Chunk, error) {
	if err := validateNodeData(n); err != nil {
		return Chunk{}, err
	}
	for _, chunk := range n.chunks {
		end, _ := checkedEnd(chunk.Offset, chunk.Size)
		if offset >= chunk.Offset && offset < end {
			return chunk, nil
		}
	}
	return Chunk{}, fmt.Errorf("%w: no chunk covers requested offset", ErrInvalidNode)
}

func selectChild(n nodeData, offset, parentEnd int64) (childData, error) {
	if err := validateNodeData(n); err != nil {
		return childData{}, err
	}
	end, ok := checkedEnd(n.start, n.size)
	if n.level == 0 || !ok || parentEnd != end {
		return childData{}, ErrInvalidNode
	}
	for _, child := range n.children {
		end, _ := checkedEnd(child.start, child.size)
		if offset >= child.start && offset < end {
			return child, nil
		}
	}
	return childData{}, fmt.Errorf("%w: no child covers requested offset", ErrInvalidNode)
}

func decodeNode(payload []byte) (nodeData, error) {
	if err := requireFields(payload, "version", "file_id", "start", "size", "level"); err != nil {
		return nodeData{}, fmt.Errorf("%w: %v", ErrInvalidNode, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nodeData{}, fmt.Errorf("%w: %v", ErrInvalidNode, err)
	}
	var wire nodeJSON
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nodeData{}, fmt.Errorf("%w: %v", ErrInvalidNode, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nodeData{}, fmt.Errorf("%w: trailing JSON data", ErrInvalidNode)
	}
	if wire.Version != manifestVersion {
		return nodeData{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidNode, wire.Version)
	}
	if wire.Level == 0 {
		if _, ok := raw["chunks"]; !ok {
			return nodeData{}, fmt.Errorf("%w: leaf has no chunks field", ErrInvalidNode)
		}
		if _, ok := raw["children"]; ok {
			return nodeData{}, fmt.Errorf("%w: leaf has children field", ErrInvalidNode)
		}
		items, err := rawArray(raw["chunks"])
		if err != nil || len(items) > MaxChunksPerPage {
			return nodeData{}, fmt.Errorf("%w: invalid chunk array", ErrInvalidNode)
		}
		for _, item := range items {
			if err := requireFields(item, "offset", "size", "stored_size", "id", "digest", "crypt_header"); err != nil {
				return nodeData{}, fmt.Errorf("%w: invalid chunk fields", ErrInvalidNode)
			}
		}
	} else {
		if _, ok := raw["children"]; !ok {
			return nodeData{}, fmt.Errorf("%w: branch has no children field", ErrInvalidNode)
		}
		if _, ok := raw["chunks"]; ok {
			return nodeData{}, fmt.Errorf("%w: branch has chunks field", ErrInvalidNode)
		}
		items, err := rawArray(raw["children"])
		if err != nil || len(items) == 0 || len(items) > MaxChunksPerPage {
			return nodeData{}, fmt.Errorf("%w: invalid child array", ErrInvalidNode)
		}
		for _, item := range items {
			if err := requireFields(item, "start", "size", "reference"); err != nil {
				return nodeData{}, fmt.Errorf("%w: invalid child fields", ErrInvalidNode)
			}
			var childRaw map[string]json.RawMessage
			if err := json.Unmarshal(item, &childRaw); err != nil {
				return nodeData{}, fmt.Errorf("%w: invalid child object", ErrInvalidNode)
			}
			if err := requireFields(childRaw["reference"], "locator", "identity", "digest"); err != nil {
				return nodeData{}, fmt.Errorf("%w: invalid child reference", ErrInvalidNode)
			}
			var refRaw map[string]json.RawMessage
			if err := json.Unmarshal(childRaw["reference"], &refRaw); err != nil {
				return nodeData{}, fmt.Errorf("%w: invalid child reference", ErrInvalidNode)
			}
			if err := requireFields(refRaw["identity"], "role", "logical_id", "generation"); err != nil {
				return nodeData{}, fmt.Errorf("%w: invalid child identity", ErrInvalidNode)
			}
		}
	}
	data := fromNodeJSON(wire)
	if err := validateNodeData(data); err != nil {
		return nodeData{}, err
	}
	return data, nil
}

func requireFields(object []byte, fields ...string) error {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(object, &values); err != nil || values == nil {
		return errors.New("expected JSON object")
	}
	for _, name := range fields {
		value, ok := values[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("missing field %q", name)
		}
	}
	return nil
}

func rawArray(raw json.RawMessage) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, ErrInvalidNode
	}
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, ErrInvalidNode
	}
	return items, nil
}

func fromNodeJSON(n nodeJSON) nodeData {
	data := nodeData{fileID: [16]byte(n.FileID), start: n.Start, size: n.Size, level: n.Level}
	if n.Chunks != nil {
		data.chunks = make([]Chunk, len(*n.Chunks))
		for i, chunk := range *n.Chunks {
			data.chunks[i] = Chunk{Offset: chunk.Offset, Size: chunk.Size, StoredSize: chunk.StoredSize, ID: chunk.ID, Digest: [32]byte(chunk.Digest), CryptHeader: [32]byte(chunk.CryptHeader)}
		}
	}
	if n.Children != nil {
		data.children = make([]childData, len(*n.Children))
		for i, child := range *n.Children {
			data.children[i] = childData{start: child.Start, size: child.Size, ref: fromReferenceJSON(child.Reference)}
		}
	}
	return data
}

func toChunkJSON(chunk Chunk) chunkJSON {
	return chunkJSON{Offset: chunk.Offset, Size: chunk.Size, StoredSize: chunk.StoredSize, ID: chunk.ID, Digest: fixed32(chunk.Digest), CryptHeader: fixed32(chunk.CryptHeader)}
}

func toReferenceJSON(ref vaultpage.Reference) referenceJSON {
	return referenceJSON{
		Locator:  ref.Locator,
		Identity: identityJSON{Role: ref.Identity.Role, LogicalID: fixed16(ref.Identity.LogicalID), Generation: ref.Identity.Generation},
		Digest:   fixed32(ref.Digest),
	}
}

func fromReferenceJSON(ref referenceJSON) vaultpage.Reference {
	return vaultpage.Reference{
		Locator:  ref.Locator,
		Identity: vaultpage.Identity{Role: ref.Identity.Role, LogicalID: [16]byte(ref.Identity.LogicalID), Generation: ref.Identity.Generation},
		Digest:   [32]byte(ref.Digest),
	}
}

func validLocator(value string) bool {
	return value != "" && len(value) <= vaultpage.MaxLocatorSize && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func checkedEnd(start, size int64) (int64, bool) {
	if start < 0 || size < 0 || size > math.MaxInt64-start {
		return 0, false
	}
	return start + size, true
}

func allZero(value []byte) bool {
	var combined byte
	for _, b := range value {
		combined |= b
	}
	return combined == 0
}

type nodeJSON struct {
	Version  uint8        `json:"version"`
	FileID   fixed16      `json:"file_id"`
	Start    int64        `json:"start"`
	Size     int64        `json:"size"`
	Level    uint8        `json:"level"`
	Chunks   *[]chunkJSON `json:"chunks,omitempty"`
	Children *[]childJSON `json:"children,omitempty"`
}

type chunkJSON struct {
	Offset      int64   `json:"offset"`
	Size        int64   `json:"size"`
	StoredSize  int64   `json:"stored_size"`
	ID          string  `json:"id"`
	Digest      fixed32 `json:"digest"`
	CryptHeader fixed32 `json:"crypt_header"`
}

type childJSON struct {
	Start     int64         `json:"start"`
	Size      int64         `json:"size"`
	Reference referenceJSON `json:"reference"`
}

type referenceJSON struct {
	Locator  string       `json:"locator"`
	Identity identityJSON `json:"identity"`
	Digest   fixed32      `json:"digest"`
}

type identityJSON struct {
	Role       vaultpage.Role `json:"role"`
	LogicalID  fixed16        `json:"logical_id"`
	Generation uint64         `json:"generation"`
}

type fixed16 [16]byte

func (value fixed16) MarshalJSON() ([]byte, error) {
	return json.Marshal([16]byte(value))
}

func (value *fixed16) UnmarshalJSON(data []byte) error {
	parsed, err := decodeFixedArray(data, len(value))
	if err != nil {
		return err
	}
	copy(value[:], parsed)
	return nil
}

type fixed32 [32]byte

func (value fixed32) MarshalJSON() ([]byte, error) {
	return json.Marshal([32]byte(value))
}

func (value *fixed32) UnmarshalJSON(data []byte) error {
	parsed, err := decodeFixedArray(data, len(value))
	if err != nil {
		return err
	}
	copy(value[:], parsed)
	return nil
}

func decodeFixedArray(data []byte, size int) ([]byte, error) {
	data = bytes.TrimSpace(data)
	if len(data) < 2 || data[0] != '[' || data[len(data)-1] != ']' {
		return nil, errors.New("expected fixed-length byte array")
	}
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil || len(values) != size {
		return nil, errors.New("invalid fixed-length byte array")
	}
	decoded := make([]byte, size)
	for i, raw := range values {
		var value uint8
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, errors.New("invalid byte value")
		}
		decoded[i] = value
	}
	return decoded, nil
}
