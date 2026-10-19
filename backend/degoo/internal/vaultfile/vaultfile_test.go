package vaultfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/rclone/rclone/backend/degoo/internal/vaultmanifest"
	"github.com/rclone/rclone/backend/degoo/internal/vaultpage"
)

type pageStore struct {
	pages map[string][]byte
	gets  int
}

func (s *pageStore) Put(_ context.Context, _ vaultpage.Identity, raw []byte, digest [32]byte) (string, error) {
	if sha256.Sum256(raw) != digest {
		return "", errors.New("bad staged digest")
	}
	id := fmt.Sprintf("page-%d", len(s.pages))
	s.pages[id] = bytes.Clone(raw)
	return id, nil
}
func (s *pageStore) Get(_ context.Context, id string) ([]byte, error) {
	s.gets++
	raw, ok := s.pages[id]
	if !ok {
		return nil, errors.New("missing page")
	}
	return bytes.Clone(raw), nil
}

func manifestFixture(t *testing.T) (*vaultmanifest.Tree, *pageStore) {
	t.Helper()
	codec, err := vaultpage.New("generated-file-test", [32]byte{1}, [16]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	store := &pageStore{pages: make(map[string][]byte)}
	repo, err := vaultpage.NewRepository(codec, store)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := vaultmanifest.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	return tree, store
}

func descriptor(offset, size int64) vaultmanifest.Chunk {
	return vaultmanifest.Chunk{Offset: offset, Size: size, StoredSize: size + 128, ID: fmt.Sprintf("data-%d", offset), Digest: [32]byte{1}, CryptHeader: [32]byte{2}}
}

type trackedCloser struct {
	io.Reader
	closed *int
	err    error
}

func (r trackedCloser) Close() error { *r.closed++; return r.err }

func TestComposeFile(t *testing.T) {
	ctx := context.Background()
	tree, store := manifestFixture(t)
	data := []byte("abcdefghijklmnopqrstuvwxyz")
	chunks := make(map[string][]byte)
	var staged, opened, closed int
	files, err := New(tree, 7, func(_ context.Context, _ [16]byte, offset, size int64, source io.Reader) (vaultmanifest.Chunk, error) {
		raw, err := io.ReadAll(source)
		if err != nil {
			return vaultmanifest.Chunk{}, err
		}
		chunk := descriptor(offset, size)
		chunks[chunk.ID] = raw
		staged++
		return chunk, nil
	}, func(_ context.Context, _ [16]byte, chunk vaultmanifest.Chunk, offset, length int64) (io.ReadCloser, error) {
		opened++
		return trackedCloser{Reader: bytes.NewReader(chunks[chunk.ID][offset : offset+length]), closed: &closed}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := files.Build(ctx, [16]byte{3}, int64(len(data)), bytes.NewReader(data))
	if err != nil || staged != 4 || manifest.Size != int64(len(data)) {
		t.Fatalf("build: %+v, staged %d, %v", manifest, staged, err)
	}
	buffer := make([]byte, 16)
	n, err := files.ReadAt(ctx, manifest, buffer, 5)
	if err != nil || n != len(buffer) || !bytes.Equal(buffer, data[5:21]) || opened != 3 || closed != opened {
		t.Fatalf("cross-chunk read: %q n=%d opened=%d closed=%d err=%v", buffer, n, opened, closed, err)
	}
	n, err = files.ReadAt(ctx, manifest, buffer, 24)
	if !errors.Is(err, io.EOF) || n != 2 || !bytes.Equal(buffer[:2], data[24:]) {
		t.Fatalf("EOF read: %q n=%d err=%v", buffer[:n], n, err)
	}
	gets := store.gets
	for _, offset := range []int64{26, 27, 1 << 62} {
		if n, err := files.ReadAt(ctx, manifest, buffer, offset); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("beyond EOF: %d %v", n, err)
		}
	}
	if n, err := files.ReadAt(ctx, manifest, nil, 0); n != 0 || err != nil || store.gets != gets {
		t.Fatalf("empty read performed I/O: %d %v gets=%d want=%d", n, err, store.gets, gets)
	}
}

// generatedReader advances without allocating logical content. The staging
// port below checks sizes only; this is splitting evidence, not data encryption.
type generatedReader struct{ remaining int64 }

func (r *generatedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.remaining))
	r.remaining -= int64(n)
	return n, nil
}

func TestLargeFileSplitsWithoutWholeFileBuffer(t *testing.T) {
	tree, _ := manifestFixture(t)
	const size int64 = 21<<30 + 17
	var count int
	var total int64
	files, err := New(tree, vaultmanifest.MaxChunkSize, func(_ context.Context, _ [16]byte, offset, length int64, source io.Reader) (vaultmanifest.Chunk, error) {
		if offset != total || length > vaultmanifest.MaxChunkSize {
			t.Fatal("unbounded or noncontiguous chunk")
		}
		n, err := io.Copy(io.Discard, source)
		if err != nil || n != length {
			t.Fatalf("source: %d/%d %v", n, length, err)
		}
		count++
		total += n
		return descriptor(offset, length), nil
	}, func(context.Context, [16]byte, vaultmanifest.Chunk, int64, int64) (io.ReadCloser, error) {
		t.Fatal("build opened a reader")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := files.Build(context.Background(), [16]byte{4}, size, &generatedReader{size})
	if err != nil || total != size || count != 22 || manifest.Size != size {
		t.Fatalf("large split: count=%d total=%d manifest=%+v err=%v", count, total, manifest, err)
	}
	chunk, err := tree.Lookup(context.Background(), manifest, size-1)
	if err != nil || chunk.Offset != 21<<30 || chunk.Size != 17 {
		t.Fatalf("last chunk: %+v %v", chunk, err)
	}
}

func TestBuildRefusesIncompleteOrInconsistentStaging(t *testing.T) {
	tree, _ := manifestFixture(t)
	stop := errors.New("stage failed")
	for _, tc := range []struct {
		name     string
		source   string
		consume  bool
		wrong    bool
		stageErr error
		want     error
	}{
		{name: "short", source: "ab", consume: true, want: ErrSourceSize},
		{name: "long", source: "abcdef", consume: true, want: ErrSourceSize},
		{name: "unconsumed", source: "abcde", want: ErrSourceSize},
		{name: "wrong-offset", source: "abcde", consume: true, wrong: true, want: ErrChunk},
		{name: "stage-error", source: "abcde", stageErr: stop, want: stop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := New(tree, 4, func(_ context.Context, _ [16]byte, offset, size int64, source io.Reader) (vaultmanifest.Chunk, error) {
				if tc.stageErr != nil {
					return vaultmanifest.Chunk{}, tc.stageErr
				}
				if tc.consume {
					if _, err := io.Copy(io.Discard, source); err != nil {
						return vaultmanifest.Chunk{}, err
					}
				}
				chunk := descriptor(offset, size)
				if tc.wrong {
					chunk.Offset++
				}
				return chunk, nil
			}, func(context.Context, [16]byte, vaultmanifest.Chunk, int64, int64) (io.ReadCloser, error) {
				return nil, stop
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := files.Build(context.Background(), [16]byte{5}, 5, stringsReader(tc.source))
			if !errors.Is(err, tc.want) || manifest != (vaultmanifest.Manifest{}) {
				t.Fatalf("got %+v %v, want %v", manifest, err, tc.want)
			}
		})
	}
}

func stringsReader(s string) io.Reader { return bytes.NewBufferString(s) }

func TestEmptyAndReadErrors(t *testing.T) {
	tree, _ := manifestFixture(t)
	ctx := context.Background()
	var staged, closed int
	stop := errors.New("close failed")
	files, err := New(tree, 4, func(_ context.Context, _ [16]byte, offset, size int64, source io.Reader) (vaultmanifest.Chunk, error) {
		staged++
		_, err := io.Copy(io.Discard, source)
		return descriptor(offset, size), err
	}, func(context.Context, [16]byte, vaultmanifest.Chunk, int64, int64) (io.ReadCloser, error) {
		return trackedCloser{Reader: bytes.NewBufferString("a"), closed: &closed, err: stop}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := files.Build(ctx, [16]byte{1}, 0, bytes.NewReader(nil))
	if err != nil || staged != 0 || empty.Size != 0 {
		t.Fatalf("empty: %+v staged=%d %v", empty, staged, err)
	}
	if _, err := files.Build(ctx, [16]byte{1}, 0, bytes.NewBufferString("x")); !errors.Is(err, ErrSourceSize) {
		t.Fatalf("nonempty zero-sized source: %v", err)
	}
	m, err := files.Build(ctx, [16]byte{1}, 4, bytes.NewBufferString("abcd"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := files.ReadAt(ctx, m, make([]byte, 4), 0)
	if n != 1 || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, stop) || closed != 1 {
		t.Fatalf("partial+close: %d %v closed=%d", n, err, closed)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := files.ReadAt(canceled, m, make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := files.Build(canceled, [16]byte{1}, 4, bytes.NewBufferString("abcd")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := files.ReadAt(ctx, m, nil, -1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
}
