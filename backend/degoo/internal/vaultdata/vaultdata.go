// Package vaultdata streams encrypted immutable chunks and authenticated ranges.
// It does not upload, publish, recover bootstrap state, or own allocation intent.
package vaultdata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/backend/degoo/internal/vaultmanifest"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
)

const innerSize = 55

var (
	magic = [8]byte{'D', 'G', 'S', 'D', 'A', 'T', '0', '2'}
	// ErrInvalidArgument reports invalid codec, identity, size or range input.
	ErrInvalidArgument = errors.New("vault data: invalid argument")
	// ErrSourceSize reports source bytes inconsistent with the declared size.
	ErrSourceSize = errors.New("vault data: source length mismatch")
	// ErrInvalidChunk reports inconsistent trusted chunk metadata.
	ErrInvalidChunk = errors.New("vault data: invalid chunk descriptor")
	// ErrHeaderMismatch reports a stream header different from the trusted manifest.
	ErrHeaderMismatch = errors.New("vault data: crypt stream header mismatch")
	// ErrIdentityMismatch reports an authenticated envelope for another chunk.
	ErrIdentityMismatch = errors.New("vault data: encrypted identity mismatch")
)

// OpenRange opens an exact ciphertext extent for a trusted immutable locator.
// The adapter verifies requested offsets, limits and total object size before
// returning its body, bounds response consumption, and honors shared provider/
// auth cooldowns and cancellation. It must not resolve through a name listing.
// Limits passed by this package are positive and clipped to expectedTotal.
type OpenRange func(ctx context.Context, locator string, offset, limit, expectedTotal int64) (io.ReadCloser, error)

// Codec uses rclone crypt's scrypt-derived authenticated content stream. Remote
// bootstrap discovery supplies salt and vaultID; local copies are not required
// recovery authority. Bootstrap discovery is outside this inactive component.
type Codec struct {
	cipher  *crypt.Cipher
	vaultID [16]byte
}

// New requires a nonempty password, nonzero salt and nonzero vault ID. Salt is
// represented to rclone crypt as lowercase hex, matching metadata page keys.
func New(password string, salt [32]byte, vaultID [16]byte) (*Codec, error) {
	if password == "" || salt == ([32]byte{}) || vaultID == ([16]byte{}) {
		return nil, ErrInvalidArgument
	}
	p, err := obscure.Obscure(password)
	if err != nil {
		return nil, err
	}
	s, err := obscure.Obscure(hex.EncodeToString(salt[:]))
	if err != nil {
		return nil, err
	}
	c, err := crypt.NewCipher(configmap.Simple{"filename_encryption": "off", "filename_encoding": "base32", "suffix": ".bin", "password": p, "password2": s})
	if err != nil {
		return nil, err
	}
	return &Codec{cipher: c, vaultID: vaultID}, nil
}

func (c *Codec) header(fileID [16]byte, offset, size int64) [innerSize]byte {
	var h [innerSize]byte
	copy(h[:8], magic[:])
	binary.BigEndian.PutUint16(h[8:10], 2)
	h[10] = 5
	copy(h[11:27], c.vaultID[:])
	copy(h[27:43], fileID[:])
	binary.BigEndian.PutUint64(h[43:51], uint64(offset))
	binary.BigEndian.PutUint32(h[51:55], uint32(size))
	return h
}

func validIdentity(fileID [16]byte, offset, size int64) bool {
	return fileID != ([16]byte{}) && offset >= 0 && size > 0 && size <= vaultmanifest.MaxChunkSize && offset <= math.MaxInt64-size
}
func validLocator(id string) bool {
	return id != "" && len(id) <= 256 && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.writer.Write(p)
	if canceled := w.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

type prefixWriter struct {
	header [32]byte
	n      int
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.n += copy(w.header[w.n:], p)
	return len(p), nil
}

// Encrypt streams exactly size source bytes into caller-owned staging output,
// adding an encrypted chunk identity envelope and fresh crypt nonce. It computes
// the complete stored SHA-256 and exact crypt framing size. A failure returns no
// descriptor; the owner must discard incomplete staging and retain any already
// issued allocation intent. Neither source nor destination is closed. Underlying
// blocking I/O must itself support cancellation if prompt interruption is needed.
func (c *Codec) Encrypt(ctx context.Context, fileID [16]byte, offset, size int64, locator string, source io.Reader, destination io.Writer) (vaultmanifest.Chunk, error) {
	if c == nil || c.cipher == nil || !validIdentity(fileID, offset, size) || !validLocator(locator) || source == nil || destination == nil {
		return vaultmanifest.Chunk{}, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return vaultmanifest.Chunk{}, err
	}
	header := c.header(fileID, offset, size)
	src := contextReader{ctx, source}
	limited := &io.LimitedReader{R: src, N: size}
	encrypted, err := c.cipher.EncryptData(io.MultiReader(bytes.NewReader(header[:]), limited))
	if err != nil {
		return vaultmanifest.Chunk{}, err
	}
	hash := sha256.New()
	prefix := new(prefixWriter)
	n, err := io.Copy(contextWriter{ctx, io.MultiWriter(destination, hash, prefix)}, encrypted)
	if err != nil {
		return vaultmanifest.Chunk{}, err
	}
	storedSize := c.cipher.EncryptedSize(innerSize + size)
	if limited.N != 0 || n != storedSize || prefix.n != 32 {
		return vaultmanifest.Chunk{}, ErrSourceSize
	}
	var extra [1]byte
	extraN, err := io.ReadFull(src, extra[:])
	if extraN != 0 {
		return vaultmanifest.Chunk{}, ErrSourceSize
	}
	if !errors.Is(err, io.EOF) {
		return vaultmanifest.Chunk{}, err
	}
	if err := ctx.Err(); err != nil {
		return vaultmanifest.Chunk{}, err
	}
	chunk := vaultmanifest.Chunk{Offset: offset, Size: size, StoredSize: storedSize, ID: strings.Clone(locator), CryptHeader: prefix.header}
	copy(chunk.Digest[:], hash.Sum(nil))
	return chunk, nil
}

// exactReader refuses a truncated extent. Returning EOF exactly at the promised
// boundary is valid, including a final partial authenticated crypt block.
type exactReader struct {
	reader    io.Reader
	remaining int64
}

func (r *exactReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	p = p[:min(int64(len(p)), r.remaining)]
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if errors.Is(err, io.EOF) && r.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

type readCloser struct {
	io.Reader
	io.Closer
}

type observedClose struct {
	io.ReadCloser
	failed *error
}

func (r observedClose) Close() error {
	err := r.ReadCloser.Close()
	if err != nil {
		*r.failed = errors.Join(*r.failed, err)
	}
	return err
}

func (c *Codec) validate(fileID [16]byte, chunk vaultmanifest.Chunk) error {
	if c == nil || c.cipher == nil || !validIdentity(fileID, chunk.Offset, chunk.Size) || !validLocator(chunk.ID) || chunk.Digest == ([32]byte{}) || chunk.CryptHeader == ([32]byte{}) || chunk.StoredSize != c.cipher.EncryptedSize(innerSize+chunk.Size) {
		return ErrInvalidChunk
	}
	return nil
}

// ReadRange authenticates the chunk's inner identity before returning a bounded
// plaintext range. Every offset-zero reopen verifies the trusted crypt header;
// authenticated blocks then bind partial reads to that nonce. Partial reads do
// not verify the complete stored SHA-256: create confirmation/reconciliation
// must do so. A zero-length range performs no object read. The caller must pin
// the immutable version until the returned reader is closed.
func (c *Codec) ReadRange(ctx context.Context, fileID [16]byte, chunk vaultmanifest.Chunk, offset, length int64, open OpenRange) (io.ReadCloser, error) {
	if err := c.validate(fileID, chunk); err != nil {
		return nil, err
	}
	if open == nil || offset < 0 || offset > chunk.Size || length < 0 || length > chunk.Size-offset {
		return nil, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if length == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	// Crypt may ignore the initial header body's Close error while reopening.
	// Refuse that next request rather than continue after an I/O failure.
	var priorCloseError error
	opener := func(ctx context.Context, storedOffset, limit int64) (io.ReadCloser, error) {
		if priorCloseError != nil {
			return nil, priorCloseError
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if storedOffset < 0 || storedOffset >= chunk.StoredSize {
			return nil, ErrInvalidChunk
		}
		limit = min(limit, chunk.StoredSize-storedOffset)
		if limit <= 0 {
			return nil, ErrInvalidChunk
		}
		body, err := open(ctx, chunk.ID, storedOffset, limit, chunk.StoredSize)
		if err != nil {
			return nil, err
		}
		if body == nil {
			return nil, ErrInvalidChunk
		}
		body = observedClose{body, &priorCloseError}
		exact := &exactReader{reader: contextReader{ctx, body}, remaining: limit}
		if storedOffset != 0 {
			return readCloser{exact, body}, nil
		}
		var actual [32]byte
		if _, err := io.ReadFull(exact, actual[:]); err != nil {
			return nil, errors.Join(err, body.Close())
		}
		if subtle.ConstantTimeCompare(actual[:], chunk.CryptHeader[:]) != 1 {
			return nil, errors.Join(ErrHeaderMismatch, body.Close())
		}
		return readCloser{io.MultiReader(bytes.NewReader(actual[:]), exact), body}, nil
	}
	identityReader, err := c.cipher.DecryptDataSeek(ctx, opener, 0, innerSize)
	if err != nil {
		return nil, err
	}
	var identity [innerSize]byte
	_, readErr := io.ReadFull(identityReader, identity[:])
	if err := errors.Join(readErr, identityReader.Close()); err != nil {
		return nil, err
	}
	expected := c.header(fileID, chunk.Offset, chunk.Size)
	if subtle.ConstantTimeCompare(identity[:], expected[:]) != 1 {
		return nil, ErrIdentityMismatch
	}
	reader, err := c.cipher.DecryptDataSeek(ctx, opener, innerSize+offset, length)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, reader.Close())
	}
	return readCloser{contextReader{ctx, reader}, reader}, nil
}
