package vaultfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/degoo/internal/vaultdata"
	"github.com/rclone/rclone/backend/degoo/internal/vaultindex"
	"github.com/rclone/rclone/backend/degoo/internal/vaultmanifest"
	"github.com/rclone/rclone/backend/degoo/internal/vaultnamespace"
	"github.com/rclone/rclone/backend/degoo/internal/vaultpage"
)

// This verifies the known authenticated snapshot's remote graph, not bootstrap
// discovery, latest-root selection, password-only recovery or provider behavior.
func TestEncryptedFileNamespaceColdGraph(t *testing.T) {
	ctx := context.Background()
	const password = "generated-integration-password"
	salt, vaultID := [32]byte{21}, [16]byte{22}
	key, fileID, rootID := [32]byte{23}, [16]byte{24}, [16]byte{25}
	pages := &pageStore{pages: make(map[string][]byte)}
	objects := make(map[string][]byte)
	makeComponents := func() (*Files, *vaultnamespace.Namespace) {
		t.Helper()
		pageCodec, err := vaultpage.New(password, salt, vaultID)
		if err != nil {
			t.Fatal(err)
		}
		repo, err := vaultpage.NewRepository(pageCodec, pages)
		if err != nil {
			t.Fatal(err)
		}
		index, err := vaultindex.New(repo)
		if err != nil {
			t.Fatal(err)
		}
		manifests, err := vaultmanifest.New(repo)
		if err != nil {
			t.Fatal(err)
		}
		namespace, err := vaultnamespace.New(index, key)
		if err != nil {
			t.Fatal(err)
		}
		dataCodec, err := vaultdata.New(password, salt, vaultID)
		if err != nil {
			t.Fatal(err)
		}
		files, err := New(manifests, 4096,
			func(ctx context.Context, id [16]byte, offset, size int64, source io.Reader) (vaultmanifest.Chunk, error) {
				locator := fmt.Sprintf("immutable-test-data-%d", len(objects))
				var stored bytes.Buffer
				chunk, err := dataCodec.Encrypt(ctx, id, offset, size, locator, source, &stored)
				if err != nil {
					return vaultmanifest.Chunk{}, err
				}
				if chunk.Digest != sha256.Sum256(stored.Bytes()) {
					t.Fatal("incorrect staged checksum")
				}
				objects[locator] = bytes.Clone(stored.Bytes())
				return chunk, nil
			}, func(ctx context.Context, id [16]byte, chunk vaultmanifest.Chunk, offset, length int64) (io.ReadCloser, error) {
				return dataCodec.ReadRange(ctx, id, chunk, offset, length, func(_ context.Context, locator string, storedOffset, limit, total int64) (io.ReadCloser, error) {
					raw, ok := objects[locator]
					if !ok || int64(len(raw)) != total || limit <= 0 || storedOffset < 0 || limit > total-storedOffset {
						return nil, fmt.Errorf("inconsistent generated extent")
					}
					return io.NopCloser(bytes.NewReader(raw[storedOffset : storedOffset+limit])), nil
				})
			})
		if err != nil {
			t.Fatal(err)
		}
		return files, namespace
	}
	files, namespace := makeComponents()
	snapshot, err := namespace.Init(ctx, rootID, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = namespace.Mkdir(ctx, snapshot, "parent", time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("generated encrypted content\x00"), 379)
	manifest, err := files.Build(ctx, fileID, int64(len(data)), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = namespace.CreateFile(ctx, snapshot, "parent/file", manifest, time.Unix(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := namespace.Lookup(ctx, snapshot, "parent")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = namespace.Rename(ctx, snapshot, "parent", "renamed", parent, time.Unix(4, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Discard all codec/tree/namespace/file instances and retain only the trusted
	// snapshot and lookup key that a future authenticated root must supply.
	files, namespace = makeComponents()
	entry, err := namespace.Lookup(ctx, snapshot, "renamed/file")
	if err != nil || entry.ID != fileID || entry.Content == nil {
		t.Fatalf("cold lookup: %+v %v", entry, err)
	}
	buffer := make([]byte, len(data))
	n, err := files.ReadAt(ctx, *entry.Content, buffer, 0)
	if err != nil || n != len(data) || !bytes.Equal(buffer, data) {
		t.Fatalf("cold encrypted read: n=%d err=%v", n, err)
	}
	if len(objects) != 3 {
		t.Fatalf("data objects=%d want 3", len(objects))
	}
}
