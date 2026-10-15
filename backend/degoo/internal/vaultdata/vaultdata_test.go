package vaultdata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/rclone/rclone/backend/degoo/internal/vaultmanifest"
)

func fixture(t *testing.T) *Codec {
	t.Helper()
	c, err := New("generated-data-password", [32]byte{1}, [16]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func encryptedFixture(t *testing.T, c *Codec, data []byte) (vaultmanifest.Chunk, []byte) {
	t.Helper()
	var stored bytes.Buffer
	chunk, err := c.Encrypt(context.Background(), [16]byte{3}, 71, int64(len(data)), "generated-data-id", bytes.NewReader(data), &stored)
	if err != nil {
		t.Fatal(err)
	}
	return chunk, stored.Bytes()
}

func memoryOpen(t *testing.T, stored []byte, expectedID string, calls *int) OpenRange {
	t.Helper()
	return func(ctx context.Context, id string, offset, limit, total int64) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		*calls++
		if id != expectedID || total != int64(len(stored)) || offset < 0 || limit <= 0 || limit > total-offset {
			t.Fatalf("unsafe range: id=%q offset=%d limit=%d total=%d", id, offset, limit, total)
		}
		return io.NopCloser(bytes.NewReader(stored[offset : offset+limit])), nil
	}
}

func TestAuthenticatedRanges(t *testing.T) {
	c := fixture(t)
	data := make([]byte, 2*65536+137)
	for i := range data {
		data[i] = byte(i*31 + 5)
	}
	chunk, stored := encryptedFixture(t, c, data)
	if chunk.Digest != sha256.Sum256(stored) || chunk.StoredSize != int64(len(stored)) || !bytes.Equal(chunk.CryptHeader[:], stored[:32]) {
		t.Fatal("incorrect stored commitment")
	}
	for _, tc := range []struct{ offset, length int64 }{{0, int64(len(data))}, {1, 17}, {65470, 113}, {65536, 777}, {int64(len(data)) - 1, 1}, {int64(len(data)), 0}} {
		calls := 0
		reader, err := c.ReadRange(context.Background(), [16]byte{3}, chunk, tc.offset, tc.length, memoryOpen(t, stored, chunk.ID, &calls))
		if err != nil {
			t.Fatal(err)
		}
		actual, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(actual, data[tc.offset:tc.offset+tc.length]) {
			t.Fatalf("range %+v got %d bytes err=%v/%v", tc, len(actual), readErr, closeErr)
		}
		if tc.length == 0 && calls != 0 {
			t.Fatal("empty range read provider")
		}
		if tc.length != 0 && calls != 3 {
			t.Fatalf("range calls=%d want 3", calls)
		}
	}
	// Equal plaintext is encrypted with a fresh nonce and distinct ciphertext.
	other, otherStored := encryptedFixture(t, c, data)
	if other.CryptHeader == chunk.CryptHeader || bytes.Equal(stored, otherStored) {
		t.Fatal("reused crypt stream nonce")
	}
}

func TestSubstitutionAndCorruptionRefused(t *testing.T) {
	c := fixture(t)
	data := bytes.Repeat([]byte("test-data-"), 15000)
	chunk, stored := encryptedFixture(t, c, data)
	_, other := encryptedFixture(t, c, data)
	for _, tc := range []struct {
		name         string
		bytes        []byte
		fileID       [16]byte
		changeHeader bool
		want         error
	}{
		{name: "valid-same-key-substitute", bytes: other, fileID: [16]byte{3}, want: ErrHeaderMismatch},
		{name: "wrong-file", bytes: stored, fileID: [16]byte{4}, want: ErrIdentityMismatch},
		{name: "replaced-body-preserved-header", bytes: other, fileID: [16]byte{3}, changeHeader: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := bytes.Clone(tc.bytes)
			if tc.changeHeader {
				copy(raw[:32], chunk.CryptHeader[:])
			}
			calls := 0
			reader, err := c.ReadRange(context.Background(), tc.fileID, chunk, 70000, 10, memoryOpen(t, raw, chunk.ID, &calls))
			if reader != nil {
				_, readErr := io.ReadAll(reader)
				err = errors.Join(err, readErr, reader.Close())
			}
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("substitution accepted or wrong error: %v", err)
			}
		})
	}
	for _, position := range []int{0, 40, 70000, len(stored) - 1} {
		raw := bytes.Clone(stored)
		raw[position] ^= 1
		calls := 0
		reader, err := c.ReadRange(context.Background(), [16]byte{3}, chunk, 0, chunk.Size, memoryOpen(t, raw, chunk.ID, &calls))
		if reader != nil {
			_, readErr := io.ReadAll(reader)
			err = errors.Join(err, readErr, reader.Close())
		}
		if err == nil {
			t.Fatalf("corruption at %d accepted", position)
		}
	}
	// A provider body truncated despite its promised extent is an I/O error.
	reader, err := c.ReadRange(context.Background(), [16]byte{3}, chunk, 0, 10, func(context.Context, string, int64, int64, int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(stored[:40])), nil
	})
	if reader != nil {
		_ = reader.Close()
	}
	if err == nil {
		t.Fatal("truncated response accepted")
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

type closeFailure struct {
	io.Reader
	err error
}

func (r closeFailure) Close() error { return r.err }

func TestHeaderReopenCloseErrorStopsRange(t *testing.T) {
	c := fixture(t)
	chunk, stored := encryptedFixture(t, c, bytes.Repeat([]byte{'x'}, 100000))
	stop := errors.New("header body close failed")
	calls := 0
	reader, err := c.ReadRange(context.Background(), [16]byte{3}, chunk, 70000, 1, func(_ context.Context, _ string, offset, limit, _ int64) (io.ReadCloser, error) {
		calls++
		var closeErr error
		if calls == 2 {
			closeErr = stop
		}
		return closeFailure{bytes.NewReader(stored[offset : offset+limit]), closeErr}, nil
	})
	if reader != nil {
		_ = reader.Close()
	}
	if !errors.Is(err, stop) || calls != 2 {
		t.Fatalf("close error lost: calls=%d err=%v", calls, err)
	}
}

func TestEncryptionExactSourceAndFailures(t *testing.T) {
	c := fixture(t)
	ctx := context.Background()
	stop := errors.New("injected I/O failure")
	for _, tc := range []struct {
		name        string
		source      io.Reader
		destination io.Writer
		want        error
	}{
		{"short", bytes.NewBufferString("ab"), io.Discard, ErrSourceSize},
		{"long", bytes.NewBufferString("abcd"), io.Discard, ErrSourceSize},
		{"source-error", failingReader{stop}, io.Discard, stop},
		{"destination-error", bytes.NewBufferString("abc"), failingWriter{stop}, stop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunk, err := c.Encrypt(ctx, [16]byte{1}, 0, 3, "data-id", tc.source, tc.destination)
			if !errors.Is(err, tc.want) || chunk != (vaultmanifest.Chunk{}) {
				t.Fatalf("got %+v %v", chunk, err)
			}
		})
	}
	for _, tc := range []struct{ offset, size int64 }{{-1, 1}, {0, 0}, {0, vaultmanifest.MaxChunkSize + 1}, {math.MaxInt64, 1}} {
		if _, err := c.Encrypt(ctx, [16]byte{1}, tc.offset, tc.size, "id", bytes.NewReader(nil), io.Discard); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid identity %+v: %v", tc, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Encrypt(canceled, [16]byte{1}, 0, 1, "id", bytes.NewBufferString("a"), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// The exact maximum framing size remains under the manifest stored-size cap.
	if c.cipher.EncryptedSize(innerSize+vaultmanifest.MaxChunkSize) > vaultmanifest.MaxStoredChunkSize {
		t.Fatal("chunk framing exceeds stored cap")
	}
}

func TestInvalidDescriptorDoesNotOpen(t *testing.T) {
	c := fixture(t)
	chunk, _ := encryptedFixture(t, c, []byte("abc"))
	for _, mutate := range []func(*vaultmanifest.Chunk){func(c *vaultmanifest.Chunk) { c.StoredSize++ }, func(c *vaultmanifest.Chunk) { c.Digest = [32]byte{} }, func(c *vaultmanifest.Chunk) { c.Offset = -1 }, func(c *vaultmanifest.Chunk) { c.CryptHeader = [32]byte{} }, func(c *vaultmanifest.Chunk) { c.ID = "bad\x00id" }} {
		bad := chunk
		mutate(&bad)
		_, err := c.ReadRange(context.Background(), [16]byte{3}, bad, 0, 1, func(context.Context, string, int64, int64, int64) (io.ReadCloser, error) {
			t.Fatal("opened invalid descriptor")
			return nil, nil
		})
		if !errors.Is(err, ErrInvalidChunk) {
			t.Fatal(err)
		}
	}
}

func TestRangeLimitsCancellationAndIdentityEnvelope(t *testing.T) {
	c := fixture(t)
	chunk, stored := encryptedFixture(t, c, bytes.Repeat([]byte{'x'}, 3*65536))
	var requested int64
	var calls int
	open := memoryOpen(t, stored, chunk.ID, &calls)
	reader, err := c.ReadRange(context.Background(), [16]byte{3}, chunk, 140000, 7,
		func(ctx context.Context, id string, offset, limit, total int64) (io.ReadCloser, error) {
			requested += limit
			return open(ctx, id, offset, limit, total)
		})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || len(actual) != 7 || requested > 2*(65536+48)+32 {
		t.Fatalf("unbounded range: len=%d requested=%d err=%v", len(actual), requested, err)
	}
	for _, r := range [][2]int64{{-1, 1}, {0, -1}, {chunk.Size, 1}, {1, chunk.Size}, {math.MaxInt64, 1}} {
		_, err := c.ReadRange(context.Background(), [16]byte{3}, chunk, r[0], r[1], func(context.Context, string, int64, int64, int64) (io.ReadCloser, error) {
			t.Fatal("opened invalid range")
			return nil, nil
		})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader, err = c.ReadRange(ctx, [16]byte{3}, chunk, 0, 1, open)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancellation: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	// A valid encrypted envelope with changed identity must still be refused.
	header := c.header([16]byte{3}, chunk.Offset, chunk.Size)
	header[10] = 1
	encrypted, err := c.cipher.EncryptData(io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(bytes.Repeat([]byte{'x'}, int(chunk.Size)))))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	bad := chunk
	bad.Digest = sha256.Sum256(raw)
	copy(bad.CryptHeader[:], raw[:32])
	_, err = c.ReadRange(context.Background(), [16]byte{3}, bad, 0, 1, memoryOpen(t, raw, bad.ID, &calls))
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("wrong authenticated role: %v", err)
	}
}
