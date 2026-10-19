// Package vaultnamespace edits immutable stable-ID filesystem snapshots.
// Callers own transaction inventory, writer serialization, reader pins and root
// publication. Proposed roots are never published by this package.
package vaultnamespace

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/degoo/internal/vaultindex"
	"github.com/rclone/rclone/backend/degoo/internal/vaultmanifest"
	"github.com/rclone/rclone/backend/degoo/internal/vaultpage"
)

// Kind identifies a logical inode type.
type Kind uint8

const (
	// File identifies a logical file backed by a manifest.
	File Kind = iota + 1
	// Directory identifies a logical directory with its own child index.
	Directory
	// MaxPathBytes caps relative namespace path bytes.
	MaxPathBytes = 4096
	// MaxNameBytes caps a single Linux name component.
	MaxNameBytes = 255
	maxPathDepth = 2048
)

var (
	// ErrInvalidArgument reports invalid operation input.
	ErrInvalidArgument = errors.New("vault namespace: invalid argument")
	// ErrInvalidPath reports an unsupported or unsafe relative path.
	ErrInvalidPath = errors.New("vault namespace: invalid relative path")
	// ErrNotFound reports an absent child link.
	ErrNotFound = errors.New("vault namespace: entry not found")
	// ErrExists reports an existing destination or logical ID.
	ErrExists = errors.New("vault namespace: entry already exists")
	// ErrNotDirectory reports a file used as a parent directory.
	ErrNotDirectory = errors.New("vault namespace: parent is not a directory")
	// ErrIsDirectory reports a directory used as a file.
	ErrIsDirectory = errors.New("vault namespace: entry is a directory")
	// ErrNotEmpty reports a directory with child links.
	ErrNotEmpty = errors.New("vault namespace: directory is not empty")
	// ErrConflict reports that an expected inode version changed.
	ErrConflict = errors.New("vault namespace: expected inode version changed")
	// ErrCycle reports a directory move into its own descendants.
	ErrCycle = errors.New("vault namespace: directory cycle")
	// ErrCorrupt reports inconsistent authenticated namespace records.
	ErrCorrupt = errors.New("vault namespace: inconsistent metadata")
	// ErrRoot reports an attempt to mutate the namespace root.
	ErrRoot = errors.New("vault namespace: root cannot be removed or replaced")
	// ErrLimit reports a revision or ancestry bound reached.
	ErrLimit = errors.New("vault namespace: metadata limit reached")
)

// Snapshot identifies one unpublished or committed inode graph. Authority and
// freshness come from the caller's publication protocol, not this value.
type Snapshot struct {
	// Root identifies the global inode index for this immutable version.
	Root vaultpage.Reference `json:"root"`
	// RootID is the stable root directory identity.
	RootID [16]byte `json:"root_id"`
}

// Entry contains metadata for one exact inode revision. Name preserves Linux
// bytes, including invalid UTF-8. Content is set only for files.
type Entry struct {
	// ID is the stable inode identity.
	ID [16]byte
	// ParentID is zero only for the root directory.
	ParentID [16]byte
	// Name preserves the exact Linux component bytes.
	Name string
	// Kind distinguishes files from directories.
	Kind Kind
	// Revision identifies the observed inode version for conditional edits.
	Revision uint64
	// Size is the logical file length; directories have zero size.
	Size int64
	// ModTime is the inode modification time.
	ModTime time.Time
	// Content is the file manifest, or nil for directories.
	Content *vaultmanifest.Manifest
}

type inode struct {
	Version  int                     `json:"version"`
	ID       [16]byte                `json:"id"`
	ParentID [16]byte                `json:"parent_id"`
	Name     []byte                  `json:"name"`
	Kind     Kind                    `json:"kind"`
	Revision uint64                  `json:"revision"`
	Size     int64                   `json:"size"`
	ModTime  time.Time               `json:"mod_time"`
	Content  *vaultmanifest.Manifest `json:"content,omitempty"`
	Children vaultpage.Reference     `json:"children"`
}
type link struct {
	Version  int      `json:"version"`
	ParentID [16]byte `json:"parent_id"`
	Name     []byte   `json:"name"`
	ID       [16]byte `json:"id"`
}

// Namespace performs copy-on-write edits using a secret lookup key supplied
// from authenticated root metadata. The key must remain stable for the vault.
type Namespace struct {
	tree *vaultindex.Tree
	key  [32]byte
}

// New binds an initialized index and a nonzero HMAC lookup key.
func New(tree *vaultindex.Tree, key [32]byte) (*Namespace, error) {
	if tree == nil || key == ([32]byte{}) {
		return nil, ErrInvalidArgument
	}
	return &Namespace{tree: tree, key: key}, nil
}

func (n *Namespace) keyFor(domain string, id [16]byte, name string) vaultindex.Key {
	mac := hmac.New(sha256.New, n.key[:])
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(id[:])
	_, _ = mac.Write([]byte(name))
	var key vaultindex.Key
	copy(key[:], mac.Sum(nil))
	return key
}
func (n *Namespace) inodeKey(id [16]byte) vaultindex.Key { return n.keyFor("inode", id, "") }
func (n *Namespace) childKey(id [16]byte, name string) vaultindex.Key {
	return n.keyFor("child", id, name)
}

func validateName(name string) error {
	if name == "" || len(name) > MaxNameBytes || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return ErrInvalidPath
	}
	return nil
}
func splitPath(remote string) ([]string, error) {
	if remote == "" {
		return nil, nil
	}
	if len(remote) > MaxPathBytes || strings.HasPrefix(remote, "/") || strings.HasSuffix(remote, "/") {
		return nil, ErrInvalidPath
	}
	parts := strings.Split(remote, "/")
	if len(parts) > maxPathDepth {
		return nil, ErrLimit
	}
	for _, part := range parts {
		if err := validateName(part); err != nil {
			return nil, err
		}
	}
	return parts, nil
}
func validateSnapshot(s Snapshot) error {
	if s.RootID == ([16]byte{}) || s.Root.Validate(vaultpage.RoleIndex) != nil {
		return ErrCorrupt
	}
	return nil
}

const maxNamespaceJSONDepth = 16

func decode(raw []byte, value any) error {
	if len(raw) > vaultindex.MaxValueSize || rejectDuplicateJSONKeys(raw) != nil {
		return ErrCorrupt
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return ErrCorrupt
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ErrCorrupt
	}
	return nil
}

// rejectDuplicateJSONKeys validates one bounded JSON value and rejects object
// member names that encoding/json could match to the same field in these records.
// Decoder.Token unescapes keys before comparison, and EqualFold matches its field
// name folding, so escaped and case-folded aliases are rejected too.
func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrCorrupt
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxNamespaceJSONDepth {
		return ErrCorrupt
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrCorrupt
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		var seen []string
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return ErrCorrupt
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrCorrupt
			}
			for _, previous := range seen {
				if strings.EqualFold(previous, key) {
					return ErrCorrupt
				}
			}
			seen = append(seen, key)
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrCorrupt
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrCorrupt
		}
	default:
		return ErrCorrupt
	}
	return nil
}

func (r inode) validate() error {
	if r.Version != 1 || r.ID == ([16]byte{}) || r.Revision == 0 || r.Size < 0 {
		return ErrCorrupt
	}
	if r.ParentID == ([16]byte{}) {
		if len(r.Name) != 0 || r.Kind != Directory {
			return ErrCorrupt
		}
	} else if validateName(string(r.Name)) != nil {
		return ErrCorrupt
	}
	switch r.Kind {
	case Directory:
		if r.Size != 0 || r.Content != nil || r.Children.Validate(vaultpage.RoleIndex) != nil {
			return ErrCorrupt
		}
	case File:
		if r.Content == nil || r.Content.FileID != r.ID || r.Content.Size != r.Size || r.Content.Root.Validate(vaultpage.RoleManifest) != nil || r.Children != (vaultpage.Reference{}) {
			return ErrCorrupt
		}
	default:
		return ErrCorrupt
	}
	return nil
}
func (r inode) entry() Entry {
	e := Entry{ID: r.ID, ParentID: r.ParentID, Name: string(r.Name), Kind: r.Kind, Revision: r.Revision, Size: r.Size, ModTime: r.ModTime}
	if r.Content != nil {
		content := *r.Content
		e.Content = &content
	}
	return e
}
func (n *Namespace) load(ctx context.Context, s Snapshot, id [16]byte) (inode, error) {
	raw, err := n.tree.Get(ctx, s.Root, n.inodeKey(id))
	if errors.Is(err, vaultindex.ErrNotFound) {
		return inode{}, ErrCorrupt
	}
	if err != nil {
		return inode{}, err
	}
	var r inode
	if decode(raw, &r) != nil || r.ID != id || r.validate() != nil {
		return inode{}, ErrCorrupt
	}
	return r, nil
}
func (n *Namespace) root(ctx context.Context, s Snapshot) (inode, error) {
	if err := validateSnapshot(s); err != nil {
		return inode{}, err
	}
	r, err := n.load(ctx, s, s.RootID)
	if err != nil {
		return inode{}, err
	}
	if r.Kind != Directory || r.ParentID != ([16]byte{}) || len(r.Name) != 0 {
		return inode{}, ErrCorrupt
	}
	return r, nil
}
func (n *Namespace) child(ctx context.Context, s Snapshot, parent inode, name string) (inode, error) {
	if parent.Kind != Directory {
		return inode{}, ErrNotDirectory
	}
	raw, err := n.tree.Get(ctx, parent.Children, n.childKey(parent.ID, name))
	if errors.Is(err, vaultindex.ErrNotFound) {
		return inode{}, ErrNotFound
	}
	if err != nil {
		return inode{}, err
	}
	var l link
	if decode(raw, &l) != nil || l.Version != 1 || l.ParentID != parent.ID || string(l.Name) != name || l.ID == ([16]byte{}) {
		return inode{}, ErrCorrupt
	}
	r, err := n.load(ctx, s, l.ID)
	if err != nil {
		return inode{}, err
	}
	if r.ParentID != parent.ID || !bytes.Equal(r.Name, l.Name) {
		return inode{}, ErrCorrupt
	}
	return r, nil
}
func (n *Namespace) resolve(ctx context.Context, s Snapshot, remote string) (inode, error) {
	parts, err := splitPath(remote)
	if err != nil {
		return inode{}, err
	}
	current, err := n.root(ctx, s)
	if err != nil {
		return inode{}, err
	}
	for _, part := range parts {
		current, err = n.child(ctx, s, current, part)
		if err != nil {
			return inode{}, err
		}
	}
	return current, nil
}
func (n *Namespace) parent(ctx context.Context, s Snapshot, remote string) (inode, string, error) {
	parts, err := splitPath(remote)
	if err != nil {
		return inode{}, "", err
	}
	if len(parts) == 0 {
		return inode{}, "", ErrRoot
	}
	parent, err := n.resolve(ctx, s, strings.Join(parts[:len(parts)-1], "/"))
	if err != nil {
		return inode{}, "", err
	}
	if parent.Kind != Directory {
		return inode{}, "", ErrNotDirectory
	}
	return parent, parts[len(parts)-1], nil
}
func (n *Namespace) write(ctx context.Context, s Snapshot, r inode) (Snapshot, error) {
	if err := r.validate(); err != nil {
		return Snapshot{}, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return Snapshot{}, err
	}
	root, err := n.tree.Put(ctx, s.Root, n.inodeKey(r.ID), b)
	if err != nil {
		return Snapshot{}, err
	}
	s.Root = root
	return s, nil
}
func (n *Namespace) writeLink(ctx context.Context, parent inode, name string, id [16]byte) (inode, error) {
	l := link{Version: 1, ParentID: parent.ID, Name: []byte(name), ID: id}
	b, err := json.Marshal(l)
	if err != nil {
		return inode{}, err
	}
	parent.Children, err = n.tree.Put(ctx, parent.Children, n.childKey(parent.ID, name), b)
	return parent, err
}
func advance(r *inode, now time.Time) error {
	if r.Revision == ^uint64(0) {
		return ErrLimit
	}
	r.Revision++
	r.ModTime = now.UTC()
	return nil
}
func randomID() ([16]byte, error) {
	var id [16]byte
	for range 8 {
		if _, err := rand.Read(id[:]); err != nil {
			return id, err
		}
		if id != ([16]byte{}) {
			return id, nil
		}
	}
	return id, ErrLimit
}
func expected(r inode, e Entry) error {
	if r.ID != e.ID || r.Revision != e.Revision || r.Kind != e.Kind {
		return ErrConflict
	}
	return nil
}

// Init stages an explicit empty root directory; it never publishes that root.
func (n *Namespace) Init(ctx context.Context, rootID [16]byte, now time.Time) (Snapshot, error) {
	if rootID == ([16]byte{}) {
		return Snapshot{}, ErrInvalidArgument
	}
	empty, err := n.tree.Empty(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	children, err := n.tree.Empty(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return n.write(ctx, Snapshot{Root: empty, RootID: rootID}, inode{Version: 1, ID: rootID, Kind: Directory, Revision: 1, ModTime: now.UTC(), Children: children})
}

// Lookup resolves a path through authenticated directory links and inode records.
func (n *Namespace) Lookup(ctx context.Context, s Snapshot, remote string) (Entry, error) {
	r, err := n.resolve(ctx, s, remote)
	if err != nil {
		return Entry{}, err
	}
	return r.entry(), nil
}

// List visits the selected directory only, verifying each child inode. Callback
// errors stop enumeration; this method never scans unrelated directories.
func (n *Namespace) List(ctx context.Context, s Snapshot, remote string, visit func(Entry) error) error {
	if visit == nil {
		return ErrInvalidArgument
	}
	dir, err := n.resolve(ctx, s, remote)
	if err != nil {
		return err
	}
	if dir.Kind != Directory {
		return ErrNotDirectory
	}
	return n.tree.Walk(ctx, dir.Children, func(key vaultindex.Key, raw []byte) error {
		var l link
		if decode(raw, &l) != nil || l.Version != 1 || l.ParentID != dir.ID || validateName(string(l.Name)) != nil || key != n.childKey(dir.ID, string(l.Name)) {
			return ErrCorrupt
		}
		child, err := n.load(ctx, s, l.ID)
		if err != nil {
			return err
		}
		if child.ParentID != dir.ID || !bytes.Equal(child.Name, l.Name) {
			return ErrCorrupt
		}
		return visit(child.entry())
	})
}
func (n *Namespace) empty(ctx context.Context, r inode) error {
	if r.Kind != Directory {
		return ErrNotDirectory
	}
	err := n.tree.Walk(ctx, r.Children, func(vaultindex.Key, []byte) error { return ErrNotEmpty })
	return err
}
func (n *Namespace) unused(ctx context.Context, s Snapshot, parent inode, name string, id [16]byte) error {
	if _, err := n.child(ctx, s, parent, name); err == nil {
		return ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if _, err := n.tree.Get(ctx, s.Root, n.inodeKey(id)); err == nil {
		return ErrExists
	} else if !errors.Is(err, vaultindex.ErrNotFound) {
		return err
	}
	return nil
}
func (n *Namespace) add(ctx context.Context, s Snapshot, parent inode, name string, r inode, now time.Time) (Snapshot, error) {
	if err := n.unused(ctx, s, parent, name, r.ID); err != nil {
		return Snapshot{}, err
	}
	r.ParentID = parent.ID
	r.Name = []byte(name)
	next, err := n.write(ctx, s, r)
	if err != nil {
		return Snapshot{}, err
	}
	parent, err = n.writeLink(ctx, parent, name, r.ID)
	if err != nil {
		return Snapshot{}, err
	}
	if err := advance(&parent, now); err != nil {
		return Snapshot{}, err
	}
	return n.write(ctx, next, parent)
}

// Mkdir stages a new directory; an existing directory is an unchanged success.
func (n *Namespace) Mkdir(ctx context.Context, s Snapshot, remote string, now time.Time) (Snapshot, error) {
	if r, err := n.resolve(ctx, s, remote); err == nil {
		if r.Kind == Directory {
			return s, nil
		}
		return Snapshot{}, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return Snapshot{}, err
	}
	parent, name, err := n.parent(ctx, s, remote)
	if err != nil {
		return Snapshot{}, err
	}
	id, err := randomID()
	if err != nil {
		return Snapshot{}, err
	}
	children, err := n.tree.Empty(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return n.add(ctx, s, parent, name, inode{Version: 1, ID: id, Kind: Directory, Revision: 1, ModTime: now.UTC(), Children: children}, now)
}

// CreateFile stages a manifest-backed file at a missing path with a new file ID.
func (n *Namespace) CreateFile(ctx context.Context, s Snapshot, remote string, content vaultmanifest.Manifest, now time.Time) (Snapshot, error) {
	if content.FileID == ([16]byte{}) || content.Size < 0 || content.Root.Validate(vaultpage.RoleManifest) != nil {
		return Snapshot{}, ErrInvalidArgument
	}
	parent, name, err := n.parent(ctx, s, remote)
	if err != nil {
		return Snapshot{}, err
	}
	return n.add(ctx, s, parent, name, inode{Version: 1, ID: content.FileID, Kind: File, Revision: 1, Size: content.Size, ModTime: now.UTC(), Content: &content}, now)
}

// ReplaceFile requires the exact observed inode revision and preserves its ID.
func (n *Namespace) ReplaceFile(ctx context.Context, s Snapshot, remote string, old Entry, content vaultmanifest.Manifest, now time.Time) (Snapshot, error) {
	r, err := n.resolve(ctx, s, remote)
	if err != nil {
		return Snapshot{}, err
	}
	if err := expected(r, old); err != nil {
		return Snapshot{}, err
	}
	if r.Kind != File {
		return Snapshot{}, ErrIsDirectory
	}
	if content.FileID != r.ID || content.Size < 0 || content.Root.Validate(vaultpage.RoleManifest) != nil {
		return Snapshot{}, ErrInvalidArgument
	}
	if err := advance(&r, now); err != nil {
		return Snapshot{}, err
	}
	r.Content = &content
	r.Size = content.Size
	return n.write(ctx, s, r)
}

// Remove requires the exact observed revision and refuses a nonempty directory.
func (n *Namespace) Remove(ctx context.Context, s Snapshot, remote string, old Entry, now time.Time) (Snapshot, error) {
	parent, name, err := n.parent(ctx, s, remote)
	if err != nil {
		return Snapshot{}, err
	}
	r, err := n.child(ctx, s, parent, name)
	if err != nil {
		return Snapshot{}, err
	}
	if err := expected(r, old); err != nil {
		return Snapshot{}, err
	}
	if r.Kind == Directory {
		if err := n.empty(ctx, r); err != nil {
			return Snapshot{}, err
		}
	}
	parent.Children, err = n.tree.Delete(ctx, parent.Children, n.childKey(parent.ID, name))
	if err != nil {
		return Snapshot{}, err
	}
	if err := advance(&parent, now); err != nil {
		return Snapshot{}, err
	}
	next, err := n.write(ctx, s, parent)
	if err != nil {
		return Snapshot{}, err
	}
	next.Root, err = n.tree.Delete(ctx, next.Root, n.inodeKey(r.ID))
	if err != nil {
		return Snapshot{}, err
	}
	return next, nil
}

func (n *Namespace) checkCycle(ctx context.Context, s Snapshot, source, destination [16]byte) error {
	visited := make(map[[16]byte]struct{})
	for range maxPathDepth + 1 {
		if destination == source {
			return ErrCycle
		}
		if _, ok := visited[destination]; ok {
			return ErrCorrupt
		}
		visited[destination] = struct{}{}
		r, err := n.load(ctx, s, destination)
		if err != nil {
			return err
		}
		if r.Kind != Directory {
			return ErrCorrupt
		}
		if destination == s.RootID {
			if r.ParentID != ([16]byte{}) {
				return ErrCorrupt
			}
			return nil
		}
		if r.ParentID == ([16]byte{}) {
			return ErrCorrupt
		}
		destination = r.ParentID
	}
	return ErrLimit
}

// Rename stages compatible destination replacement and stable-ID movement.
// Descendant records and file contents remain unchanged. The expected source
// revision prevents an old handle moving a newer replacement.
func (n *Namespace) Rename(ctx context.Context, s Snapshot, src, dst string, old Entry, now time.Time) (Snapshot, error) {
	source, err := n.resolve(ctx, s, src)
	if err != nil {
		return Snapshot{}, err
	}
	if err := expected(source, old); err != nil {
		return Snapshot{}, err
	}
	if src == "" || dst == "" {
		return Snapshot{}, ErrRoot
	}
	if src == dst {
		return s, nil
	}
	from, srcName, err := n.parent(ctx, s, src)
	if err != nil {
		return Snapshot{}, err
	}
	to, dstName, err := n.parent(ctx, s, dst)
	if err != nil {
		return Snapshot{}, err
	}
	if source.Kind == Directory {
		if err := n.checkCycle(ctx, s, source.ID, to.ID); err != nil {
			return Snapshot{}, err
		}
	}
	target, targetErr := n.child(ctx, s, to, dstName)
	if targetErr != nil && !errors.Is(targetErr, ErrNotFound) {
		return Snapshot{}, targetErr
	}
	replace := targetErr == nil
	if replace {
		if target.ID == source.ID {
			return Snapshot{}, ErrCorrupt
		}
		if target.Kind != source.Kind {
			if target.Kind == Directory {
				return Snapshot{}, ErrIsDirectory
			}
			return Snapshot{}, ErrNotDirectory
		}
		if target.Kind == Directory {
			if err := n.empty(ctx, target); err != nil {
				return Snapshot{}, err
			}
		}
	}
	if err := advance(&source, source.ModTime); err != nil {
		return Snapshot{}, err
	}
	source.Name = []byte(dstName)
	source.ParentID = to.ID
	from.Children, err = n.tree.Delete(ctx, from.Children, n.childKey(from.ID, srcName))
	if err != nil {
		return Snapshot{}, err
	}
	if from.ID == to.ID {
		to = from
	}
	if replace {
		to.Children, err = n.tree.Delete(ctx, to.Children, n.childKey(to.ID, dstName))
		if err != nil {
			return Snapshot{}, err
		}
	}
	to, err = n.writeLink(ctx, to, dstName, source.ID)
	if err != nil {
		return Snapshot{}, err
	}
	if err := advance(&to, now); err != nil {
		return Snapshot{}, err
	}
	next := s
	if from.ID != to.ID {
		if err := advance(&from, now); err != nil {
			return Snapshot{}, err
		}
		next, err = n.write(ctx, next, from)
		if err != nil {
			return Snapshot{}, err
		}
	}
	next, err = n.write(ctx, next, to)
	if err != nil {
		return Snapshot{}, err
	}
	next, err = n.write(ctx, next, source)
	if err != nil {
		return Snapshot{}, err
	}
	if replace {
		next.Root, err = n.tree.Delete(ctx, next.Root, n.inodeKey(target.ID))
		if err != nil {
			return Snapshot{}, err
		}
	}
	return next, nil
}

// String returns a description without exposing the secret key or user names.
func (n *Namespace) String() string { return "encrypted stable-ID namespace" }
