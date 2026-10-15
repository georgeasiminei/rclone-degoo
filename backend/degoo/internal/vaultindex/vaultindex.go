// Package vaultindex implements an inactive encrypted copy-on-write radix index.
//
// The package stores caller-supplied keyed digests and opaque records through
// vaultpage.Repository. It does not define names, publish roots, retry writes,
// or collect pages left staged by a failed operation.
package vaultindex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/rclone/rclone/backend/degoo/internal/vaultpage"
)

const (
	// MaxValueSize bounds one opaque index value.
	MaxValueSize = 8 << 10
	// MaxLeafRecords bounds the number of records in one leaf page.
	MaxLeafRecords = 64
	indexVersion   = 1
	keyNibbles     = 64
)

var (
	// ErrNotFound reports that a key is not present in the selected root.
	ErrNotFound = errors.New("vault index: key not found")
	// ErrInvalidRoot reports an invalid or wrong-role index root reference.
	ErrInvalidRoot = errors.New("vault index: invalid root reference")
	// ErrInvalidNode reports malformed or inconsistent authenticated page data.
	ErrInvalidNode = errors.New("vault index: invalid index page")
	// ErrValueTooLarge reports an opaque value over MaxValueSize.
	ErrValueTooLarge = errors.New("vault index: value exceeds limit")
	// ErrPayloadTooLarge reports a page that cannot fit the repository budget.
	ErrPayloadTooLarge = errors.New("vault index: page exceeds payload limit")
	// ErrDepthLimit reports an unsplittable page at the maximum key depth.
	ErrDepthLimit = errors.New("vault index: maximum radix depth exceeded")
	// ErrInvalidTree reports a missing repository or callback.
	ErrInvalidTree = errors.New("vault index: invalid tree")
)

// Key is a caller-derived 256-bit lookup key. The namespace layer is
// responsible for deriving it with a keyed digest rather than a plain name hash.
type Key [32]byte

type pageKind string

const (
	leafPage   pageKind = "leaf"
	branchPage pageKind = "branch"
)

type record struct {
	Key   Key    `json:"key"`
	Value []byte `json:"value"`
}

// page is the encrypted JSON payload for one radix node. Both shape fields are
// always present so a leaf/branch mix or omitted child vector is rejected.
type page struct {
	Version  uint8                  `json:"version"`
	Kind     pageKind               `json:"kind"`
	Prefix   string                 `json:"prefix"`
	Depth    uint8                  `json:"depth"`
	Records  []record               `json:"records"`
	Children []*vaultpage.Reference `json:"children"`
}

// Tree reads and writes immutable index pages through a verified repository.
type Tree struct {
	repo       *vaultpage.Repository
	maxPayload int
}

// New binds a tree to an initialized page repository.
func New(repo *vaultpage.Repository) (*Tree, error) {
	if repo == nil || repo.MaxPayload() <= 0 {
		return nil, ErrInvalidTree
	}
	return &Tree{repo: repo, maxPayload: repo.MaxPayload()}, nil
}

// Empty explicitly creates an empty index root. The caller's transaction owns
// publication and any staged page left by a failed create.
func (t *Tree) Empty(ctx context.Context) (vaultpage.Reference, error) {
	if err := t.check(); err != nil {
		return vaultpage.Reference{}, err
	}
	if err := ctx.Err(); err != nil {
		return vaultpage.Reference{}, err
	}
	return t.writePage(ctx, page{
		Version: indexVersion, Kind: leafPage, Prefix: "", Depth: 0,
		Records: []record{}, Children: []*vaultpage.Reference{},
	})
}

// Get returns a copy of the value for key from root.
func (t *Tree) Get(ctx context.Context, root vaultpage.Reference, key Key) ([]byte, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	if err := validateRoot(root); err != nil {
		return nil, err
	}
	currentRef := root
	var expected *edge
	for reads := 0; reads <= keyNibbles; reads++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := t.readPage(ctx, currentRef, expected, reads == 0)
		if err != nil {
			return nil, err
		}
		if !keyHasPrefix(key, n.Prefix) {
			return nil, ErrNotFound
		}
		if n.Kind == leafPage {
			i := sort.Search(len(n.Records), func(i int) bool {
				return bytes.Compare(n.Records[i].Key[:], key[:]) >= 0
			})
			if i == len(n.Records) || n.Records[i].Key != key {
				return nil, ErrNotFound
			}
			return bytes.Clone(n.Records[i].Value), nil
		}
		if int(n.Depth) >= keyNibbles {
			return nil, ErrInvalidNode
		}
		slot := nibble(key, int(n.Depth))
		child := n.Children[slot]
		if child == nil {
			return nil, ErrNotFound
		}
		currentRef = *child
		expected = childEdge{parent: n, slot: slot}.ptr()
	}
	return nil, ErrDepthLimit
}

// Put returns a new root containing value for key. It copies only the changed
// path and any descendants created while splitting an overflowing leaf.
func (t *Tree) Put(ctx context.Context, root vaultpage.Reference, key Key, value []byte) (vaultpage.Reference, error) {
	if err := t.check(); err != nil {
		return vaultpage.Reference{}, err
	}
	if err := validateRoot(root); err != nil {
		return vaultpage.Reference{}, err
	}
	if len(value) > MaxValueSize {
		return vaultpage.Reference{}, ErrValueTooLarge
	}
	if err := ctx.Err(); err != nil {
		return vaultpage.Reference{}, err
	}
	n, err := t.readPage(ctx, root, nil, true)
	if err != nil {
		return vaultpage.Reference{}, err
	}
	newRef, _, changed, err := t.insert(ctx, root, n, record{Key: key, Value: bytes.Clone(value)})
	if err != nil {
		return vaultpage.Reference{}, err
	}
	if !changed {
		return root, nil
	}
	return newRef, nil
}

// Delete returns a root without key. Deleting a missing key is a no-op and
// returns root unchanged.
func (t *Tree) Delete(ctx context.Context, root vaultpage.Reference, key Key) (vaultpage.Reference, error) {
	if err := t.check(); err != nil {
		return vaultpage.Reference{}, err
	}
	if err := validateRoot(root); err != nil {
		return vaultpage.Reference{}, err
	}
	if err := ctx.Err(); err != nil {
		return vaultpage.Reference{}, err
	}
	n, err := t.readPage(ctx, root, nil, true)
	if err != nil {
		return vaultpage.Reference{}, err
	}
	newRef, _, changed, err := t.delete(ctx, root, n, key, true)
	if err != nil {
		return vaultpage.Reference{}, err
	}
	if !changed {
		return root, nil
	}
	return newRef, nil
}

// Walk streams all records in key order and validates every referenced page.
// Its recursion depth is bounded by the 64 key nibbles.
func (t *Tree) Walk(ctx context.Context, root vaultpage.Reference, visit func(Key, []byte) error) error {
	if err := t.check(); err != nil {
		return err
	}
	if visit == nil {
		return ErrInvalidTree
	}
	if err := validateRoot(root); err != nil {
		return err
	}
	var walk func(vaultpage.Reference, *edge, bool) error
	walk = func(ref vaultpage.Reference, expected *edge, rootPage bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := t.readPage(ctx, ref, expected, rootPage)
		if err != nil {
			return err
		}
		if n.Kind == leafPage {
			for _, item := range n.Records {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := visit(item.Key, bytes.Clone(item.Value)); err != nil {
					return err
				}
			}
			return nil
		}
		for slot, child := range n.Children {
			if child == nil {
				continue
			}
			if err := walk(*child, childEdge{parent: n, slot: slot}.ptr(), false); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root, nil, true)
}

type edge struct {
	parentPrefix string
	parentDepth  int
	slot         int
}

type childEdge struct {
	parent *page
	slot   int
}

func (e childEdge) ptr() *edge {
	return &edge{parentPrefix: e.parent.Prefix, parentDepth: int(e.parent.Depth), slot: e.slot}
}

func (t *Tree) check() error {
	if t == nil || t.repo == nil || t.maxPayload <= 0 {
		return ErrInvalidTree
	}
	return nil
}

func validateRoot(ref vaultpage.Reference) error {
	if err := ref.Validate(vaultpage.RoleIndex); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRoot, err)
	}
	return nil
}

func (t *Tree) readPage(ctx context.Context, ref vaultpage.Reference, expected *edge, root bool) (*page, error) {
	if err := ref.Validate(vaultpage.RoleIndex); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidNode, err)
	}
	payload, err := t.repo.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("vault index: read page: %w", err)
	}
	n, err := decodePage(payload, t.maxPayload, root)
	if err != nil {
		return nil, err
	}
	if expected != nil {
		if int(n.Depth) <= expected.parentDepth || expected.parentDepth >= keyNibbles || expected.slot < 0 || expected.slot >= 16 {
			return nil, fmt.Errorf("%w: child depth does not advance", ErrInvalidNode)
		}
		want := expected.parentPrefix + hexDigit(expected.slot)
		if len(n.Prefix) < len(want) || !strings.HasPrefix(n.Prefix, want) {
			return nil, fmt.Errorf("%w: child prefix does not match parent slot", ErrInvalidNode)
		}
	}
	return &n, nil
}

func decodePage(payload []byte, maxPayload int, root bool) (page, error) {
	var n page
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&n); err != nil {
		return page{}, fmt.Errorf("%w: decode JSON: %v", ErrInvalidNode, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return page{}, fmt.Errorf("%w: trailing JSON value", ErrInvalidNode)
		}
		return page{}, fmt.Errorf("%w: trailing JSON: %v", ErrInvalidNode, err)
	}
	canonical, err := json.Marshal(n)
	if err != nil || !bytes.Equal(canonical, payload) {
		return page{}, fmt.Errorf("%w: non-canonical JSON", ErrInvalidNode)
	}
	if err := validatePage(n, maxPayload, root); err != nil {
		return page{}, err
	}
	return n, nil
}

func validatePage(n page, maxPayload int, root bool) error {
	if n.Version != indexVersion || int(n.Depth) > keyNibbles || len(n.Prefix) != int(n.Depth) || !normalizedPrefix(n.Prefix) {
		return fmt.Errorf("%w: version or normalized prefix", ErrInvalidNode)
	}
	if n.Records == nil || n.Children == nil {
		return fmt.Errorf("%w: missing shape fields", ErrInvalidNode)
	}
	switch n.Kind {
	case leafPage:
		if len(n.Children) != 0 || len(n.Records) > MaxLeafRecords || (len(n.Records) == 0 && !root) {
			return fmt.Errorf("%w: invalid leaf shape", ErrInvalidNode)
		}
		for i, item := range n.Records {
			if len(item.Value) > MaxValueSize || !keyHasPrefix(item.Key, n.Prefix) {
				return fmt.Errorf("%w: leaf record outside prefix or over value cap", ErrInvalidNode)
			}
			if i > 0 && bytes.Compare(n.Records[i-1].Key[:], item.Key[:]) >= 0 {
				return fmt.Errorf("%w: leaf keys are not strictly sorted", ErrInvalidNode)
			}
		}
		if len(n.Records) > 0 && commonDepth(n.Records) != int(n.Depth) {
			return fmt.Errorf("%w: leaf prefix is not normalized", ErrInvalidNode)
		}
		if len(n.Records) == 0 && (n.Prefix != "" || n.Depth != 0) {
			return fmt.Errorf("%w: empty root must have an empty prefix", ErrInvalidNode)
		}
	case branchPage:
		if len(n.Records) != 0 || len(n.Children) != 16 || int(n.Depth) >= keyNibbles {
			return fmt.Errorf("%w: invalid branch shape", ErrInvalidNode)
		}
		present := 0
		identities := make(map[vaultpage.Identity]struct{}, 16)
		locators := make(map[string]struct{}, 16)
		for _, child := range n.Children {
			if child == nil {
				continue
			}
			present++
			if err := child.Validate(vaultpage.RoleIndex); err != nil {
				return fmt.Errorf("%w: invalid child reference: %v", ErrInvalidNode, err)
			}
			if _, exists := identities[child.Identity]; exists {
				return fmt.Errorf("%w: child identity is repeated", ErrInvalidNode)
			}
			if _, exists := locators[child.Locator]; exists {
				return fmt.Errorf("%w: child locator is repeated", ErrInvalidNode)
			}
			identities[child.Identity] = struct{}{}
			locators[child.Locator] = struct{}{}
		}
		if present < 2 {
			return fmt.Errorf("%w: branch has fewer than two children", ErrInvalidNode)
		}
	default:
		return fmt.Errorf("%w: unknown page kind", ErrInvalidNode)
	}
	encoded, err := json.Marshal(n)
	if err != nil || len(encoded) > maxPayload {
		return ErrPayloadTooLarge
	}
	return nil
}

func normalizedPrefix(prefix string) bool {
	for _, c := range prefix {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (t *Tree) insert(ctx context.Context, ref vaultpage.Reference, n *page, item record) (vaultpage.Reference, *page, bool, error) {
	if err := ctx.Err(); err != nil {
		return vaultpage.Reference{}, nil, false, err
	}
	if !keyHasPrefix(item.Key, n.Prefix) {
		return t.splitOutside(ctx, ref, n, item)
	}
	if n.Kind == leafPage {
		i := sort.Search(len(n.Records), func(i int) bool {
			return bytes.Compare(n.Records[i].Key[:], item.Key[:]) >= 0
		})
		records := make([]record, len(n.Records))
		copy(records, n.Records)
		if i < len(records) && records[i].Key == item.Key {
			if bytes.Equal(records[i].Value, item.Value) {
				return ref, n, false, nil
			}
			records[i] = item
		} else {
			records = append(records, record{})
			copy(records[i+1:], records[i:])
			records[i] = item
		}
		newRef, newPage, err := t.build(ctx, records, n.Prefix, int(n.Depth))
		return newRef, newPage, true, err
	}
	if int(n.Depth) >= keyNibbles {
		return vaultpage.Reference{}, nil, false, ErrDepthLimit
	}
	slot := nibble(item.Key, int(n.Depth))
	children := cloneChildren(n.Children)
	if children[slot] == nil {
		leaf := []record{item}
		childRef, _, err := t.build(ctx, leaf, n.Prefix+hexDigit(slot), int(n.Depth)+1)
		if err != nil {
			return vaultpage.Reference{}, nil, false, err
		}
		children[slot] = &childRef
		updated := clonePage(n)
		updated.Children = children
		newRef, err := t.writePage(ctx, updated)
		return newRef, &updated, true, err
	}
	childRef := *children[slot]
	child, err := t.readPage(ctx, childRef, childEdge{parent: n, slot: slot}.ptr(), false)
	if err != nil {
		return vaultpage.Reference{}, nil, false, err
	}
	newChildRef, _, changed, err := t.insert(ctx, childRef, child, item)
	if err != nil || !changed {
		return newChildRef, n, changed, err
	}
	children[slot] = &newChildRef
	updated := clonePage(n)
	updated.Children = children
	newRef, err := t.writePage(ctx, updated)
	return newRef, &updated, true, err
}

func (t *Tree) splitOutside(ctx context.Context, oldRef vaultpage.Reference, old *page, item record) (vaultpage.Reference, *page, bool, error) {
	depth := commonPrefixWithKey(old.Prefix, item.Key)
	if depth >= int(old.Depth) || depth >= keyNibbles {
		return vaultpage.Reference{}, nil, false, ErrDepthLimit
	}
	oldSlot := nibbleFromPrefix(old.Prefix, depth)
	newSlot := nibble(item.Key, depth)
	if oldSlot == newSlot {
		return vaultpage.Reference{}, nil, false, ErrInvalidNode
	}
	newChild, _, err := t.build(ctx, []record{item}, keyPrefix(item.Key, depth+1), depth+1)
	if err != nil {
		return vaultpage.Reference{}, nil, false, err
	}
	children := emptyChildren()
	children[oldSlot] = &oldRef
	children[newSlot] = &newChild
	branch := page{Version: indexVersion, Kind: branchPage, Prefix: keyPrefix(item.Key, depth), Depth: uint8(depth), Records: []record{}, Children: children}
	newRef, err := t.writePage(ctx, branch)
	return newRef, &branch, true, err
}

func (t *Tree) build(ctx context.Context, records []record, expectedPrefix string, expectedDepth int) (vaultpage.Reference, *page, error) {
	if err := ctx.Err(); err != nil {
		return vaultpage.Reference{}, nil, err
	}
	if len(records) == 0 || expectedDepth < 0 || expectedDepth > keyNibbles || len(expectedPrefix) != expectedDepth || !normalizedPrefix(expectedPrefix) {
		return vaultpage.Reference{}, nil, ErrInvalidNode
	}
	for i, item := range records {
		if len(item.Value) > MaxValueSize || !keyHasPrefix(item.Key, expectedPrefix) || (i > 0 && bytes.Compare(records[i-1].Key[:], item.Key[:]) >= 0) {
			return vaultpage.Reference{}, nil, ErrInvalidNode
		}
	}
	depth := commonDepth(records)
	prefix := keyPrefix(records[0].Key, depth)
	candidate := page{Version: indexVersion, Kind: leafPage, Prefix: prefix, Depth: uint8(depth), Records: records, Children: []*vaultpage.Reference{}}
	if len(records) <= MaxLeafRecords {
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return vaultpage.Reference{}, nil, fmt.Errorf("vault index: encode leaf: %w", err)
		}
		if len(encoded) <= t.maxPayload {
			ref, err := t.writePage(ctx, candidate)
			return ref, &candidate, err
		}
	}
	if depth >= keyNibbles {
		return vaultpage.Reference{}, nil, ErrDepthLimit
	}
	if len(records) < 2 {
		return vaultpage.Reference{}, nil, ErrPayloadTooLarge
	}
	children := emptyChildren()
	start := 0
	for start < len(records) {
		slot := nibble(records[start].Key, depth)
		end := start + 1
		for end < len(records) && nibble(records[end].Key, depth) == slot {
			end++
		}
		childPrefix := prefix + hexDigit(slot)
		childRef, _, err := t.build(ctx, records[start:end], childPrefix, depth+1)
		if err != nil {
			return vaultpage.Reference{}, nil, err
		}
		children[slot] = &childRef
		start = end
	}
	branch := page{Version: indexVersion, Kind: branchPage, Prefix: prefix, Depth: uint8(depth), Records: []record{}, Children: children}
	ref, err := t.writePage(ctx, branch)
	return ref, &branch, err
}

func (t *Tree) delete(ctx context.Context, ref vaultpage.Reference, n *page, key Key, root bool) (vaultpage.Reference, *page, bool, error) {
	if err := ctx.Err(); err != nil {
		return vaultpage.Reference{}, nil, false, err
	}
	if !keyHasPrefix(key, n.Prefix) {
		return ref, n, false, nil
	}
	if n.Kind == leafPage {
		i := sort.Search(len(n.Records), func(i int) bool {
			return bytes.Compare(n.Records[i].Key[:], key[:]) >= 0
		})
		if i == len(n.Records) || n.Records[i].Key != key {
			return ref, n, false, nil
		}
		records := make([]record, 0, len(n.Records)-1)
		records = append(records, n.Records[:i]...)
		records = append(records, n.Records[i+1:]...)
		if len(records) == 0 {
			if !root {
				return vaultpage.Reference{}, nil, true, nil
			}
			empty := page{Version: indexVersion, Kind: leafPage, Prefix: "", Depth: 0, Records: []record{}, Children: []*vaultpage.Reference{}}
			newRef, err := t.writePage(ctx, empty)
			return newRef, &empty, true, err
		}
		newRef, newPage, err := t.build(ctx, records, n.Prefix, int(n.Depth))
		return newRef, newPage, true, err
	}
	if int(n.Depth) >= keyNibbles {
		return vaultpage.Reference{}, nil, false, ErrInvalidNode
	}
	slot := nibble(key, int(n.Depth))
	if n.Children[slot] == nil {
		return ref, n, false, nil
	}
	childRef := *n.Children[slot]
	child, err := t.readPage(ctx, childRef, childEdge{parent: n, slot: slot}.ptr(), false)
	if err != nil {
		return vaultpage.Reference{}, nil, false, err
	}
	if !keyHasPrefix(key, child.Prefix) {
		return ref, n, false, nil
	}
	newChildRef, newChild, changed, err := t.delete(ctx, childRef, child, key, false)
	if err != nil || !changed {
		return newChildRef, n, changed, err
	}
	updated := clonePage(n)
	updated.Children = cloneChildren(n.Children)
	if newChild == nil {
		updated.Children[slot] = nil
	} else {
		updated.Children[slot] = &newChildRef
	}
	present := 0
	var onlyRef vaultpage.Reference
	var onlySlot int
	for i, childRef := range updated.Children {
		if childRef == nil {
			continue
		}
		present++
		if present == 1 {
			onlyRef = *childRef
			onlySlot = i
		}
	}
	if present == 0 {
		if !root {
			return vaultpage.Reference{}, nil, true, nil
		}
		empty := page{Version: indexVersion, Kind: leafPage, Prefix: "", Depth: 0, Records: []record{}, Children: []*vaultpage.Reference{}}
		newRef, err := t.writePage(ctx, empty)
		return newRef, &empty, true, err
	}
	cached := [16]*page{}
	if newChild != nil {
		cached[slot] = newChild
	}
	if present == 1 {
		if onlySlot == slot && newChild != nil {
			if newChild.Kind == branchPage {
				return onlyRef, newChild, true, nil
			}
		} else {
			cached[onlySlot], err = t.readPage(ctx, onlyRef, childEdge{parent: &updated, slot: onlySlot}.ptr(), false)
			if err != nil {
				return vaultpage.Reference{}, nil, false, err
			}
			if cached[onlySlot].Kind == branchPage {
				return onlyRef, cached[onlySlot], true, nil
			}
		}
	}
	merged, allLeaves, err := t.mergeLeaves(ctx, &updated, cached)
	if err != nil {
		return vaultpage.Reference{}, nil, false, err
	}
	if allLeaves {
		mergedRef, mergedPage, buildErr := t.build(ctx, merged, updated.Prefix, int(updated.Depth))
		if buildErr == nil {
			return mergedRef, mergedPage, true, nil
		}
		if !errors.Is(buildErr, ErrPayloadTooLarge) && !errors.Is(buildErr, ErrDepthLimit) {
			return vaultpage.Reference{}, nil, false, buildErr
		}
	}
	newRef, err := t.writePage(ctx, updated)
	return newRef, &updated, true, err
}

func (t *Tree) mergeLeaves(ctx context.Context, branch *page, cached [16]*page) ([]record, bool, error) {
	merged := make([]record, 0)
	for slot, childRef := range branch.Children {
		if childRef == nil {
			continue
		}
		child := cached[slot]
		if child == nil {
			var err error
			child, err = t.readPage(ctx, *childRef, childEdge{parent: branch, slot: slot}.ptr(), false)
			if err != nil {
				return nil, false, err
			}
		}
		if child.Kind != leafPage {
			return nil, false, nil
		}
		merged = append(merged, child.Records...)
	}
	sort.Slice(merged, func(i, j int) bool {
		return bytes.Compare(merged[i].Key[:], merged[j].Key[:]) < 0
	})
	if len(merged) == 0 || len(merged) > MaxLeafRecords {
		return nil, false, nil
	}
	for i := 1; i < len(merged); i++ {
		if bytes.Compare(merged[i-1].Key[:], merged[i].Key[:]) >= 0 {
			return nil, false, ErrInvalidNode
		}
	}
	depth := commonDepth(merged)
	candidate := page{Version: indexVersion, Kind: leafPage, Prefix: keyPrefix(merged[0].Key, depth), Depth: uint8(depth), Records: merged, Children: []*vaultpage.Reference{}}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return nil, false, fmt.Errorf("vault index: encode merged leaf: %w", err)
	}
	return merged, len(encoded) <= t.maxPayload, nil
}

func (t *Tree) writePage(ctx context.Context, n page) (vaultpage.Reference, error) {
	if err := ctx.Err(); err != nil {
		return vaultpage.Reference{}, err
	}
	if err := validatePage(n, t.maxPayload, len(n.Records) == 0 && n.Kind == leafPage); err != nil {
		return vaultpage.Reference{}, err
	}
	encoded, err := json.Marshal(n)
	if err != nil {
		return vaultpage.Reference{}, fmt.Errorf("vault index: encode page: %w", err)
	}
	var logicalID [16]byte
	if _, err := rand.Read(logicalID[:]); err != nil {
		return vaultpage.Reference{}, fmt.Errorf("vault index: generate page ID: %w", err)
	}
	if allZero(logicalID[:]) {
		return vaultpage.Reference{}, errors.New("vault index: random page ID was zero")
	}
	identity := vaultpage.Identity{Role: vaultpage.RoleIndex, LogicalID: logicalID, Generation: 1}
	return t.repo.Put(ctx, identity, encoded)
}

func clonePage(n *page) page {
	return page{Version: n.Version, Kind: n.Kind, Prefix: n.Prefix, Depth: n.Depth, Records: n.Records, Children: n.Children}
}

func cloneChildren(children []*vaultpage.Reference) []*vaultpage.Reference {
	cloned := make([]*vaultpage.Reference, len(children))
	for i, child := range children {
		if child != nil {
			ref := *child
			cloned[i] = &ref
		}
	}
	return cloned
}

func emptyChildren() []*vaultpage.Reference { return make([]*vaultpage.Reference, 16) }

func commonDepth(records []record) int {
	if len(records) == 0 {
		return 0
	}
	first := records[0].Key
	depth := keyNibbles
	for _, item := range records[1:] {
		for depth > 0 && !keyPrefixMatches(item.Key, first, depth) {
			depth--
		}
	}
	return depth
}

func commonPrefixWithKey(prefix string, key Key) int {
	depth := len(prefix)
	for i := 0; i < depth; i++ {
		if nibble(key, i) != nibbleFromPrefix(prefix, i) {
			return i
		}
	}
	return depth
}

func keyPrefixMatches(key, first Key, depth int) bool {
	for i := 0; i < depth; i++ {
		if nibble(key, i) != nibble(first, i) {
			return false
		}
	}
	return true
}

func keyHasPrefix(key Key, prefix string) bool {
	if len(prefix) > keyNibbles {
		return false
	}
	for i := range prefix {
		if nibble(key, i) != nibbleFromPrefix(prefix, i) {
			return false
		}
	}
	return true
}

func keyPrefix(key Key, depth int) string {
	const digits = "0123456789abcdef"
	if depth <= 0 {
		return ""
	}
	if depth > keyNibbles {
		depth = keyNibbles
	}
	result := make([]byte, depth)
	for i := 0; i < depth; i++ {
		result[i] = digits[nibble(key, i)]
	}
	return string(result)
}

func nibble(key Key, depth int) int {
	b := key[depth/2]
	if depth%2 == 0 {
		return int(b >> 4)
	}
	return int(b & 0x0f)
}

func nibbleFromPrefix(prefix string, depth int) int {
	c := prefix[depth]
	if c >= '0' && c <= '9' {
		return int(c - '0')
	}
	return int(c-'a') + 10
}

func hexDigit(value int) string {
	return string("0123456789abcdef"[value])
}

func allZero(value []byte) bool {
	var combined byte
	for _, b := range value {
		combined |= b
	}
	return combined == 0
}
