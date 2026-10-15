// Package vaultfile composes bounded data chunks and encrypted manifests.
// Callers own pending source durability, allocation intents, authenticated chunk
// I/O, transaction inventory, reader pins and publication of the returned manifest.
package vaultfile

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/rclone/rclone/backend/degoo/internal/vaultmanifest"
)

var (
	// ErrInvalidArgument reports invalid file operation input.
	ErrInvalidArgument = errors.New("vault file: invalid argument")
	// ErrSourceSize reports source bytes inconsistent with the declared size.
	ErrSourceSize = errors.New("vault file: source length mismatch")
	// ErrChunk reports a staging result inconsistent with the requested chunk.
	ErrChunk = errors.New("vault file: inconsistent staged chunk")
)

// StageChunk consumes exactly size plaintext bytes and returns a confirmed
// encrypted object descriptor. Its owner persists intent before remote effects,
// retains unknown outcomes and failed staging, and never reuses physical IDs.
// It must bind authenticated chunk identity to fileID, offset and size.
type StageChunk func(ctx context.Context, fileID [16]byte, offset, size int64, source io.Reader) (vaultmanifest.Chunk, error)

// OpenRange opens an authenticated plaintext interval relative to chunk start.
// It verifies the trusted descriptor and expected file ID before returning data.
// Its owner honors provider/auth cooldowns and cancellation, without implicit
// write retries or listing-based substitution. Close releases the range reader.
type OpenRange func(ctx context.Context, fileID [16]byte, chunk vaultmanifest.Chunk, offset, length int64) (io.ReadCloser, error)

// Files streams logical file contents through a bounded manifest tree.
type Files struct {
	manifests *vaultmanifest.Tree
	chunkSize int64
	stage     StageChunk
	open      OpenRange
}

// New binds a manifest tree, chunk size in 1..1 GiB and both I/O ports.
func New(manifests *vaultmanifest.Tree, chunkSize int64, stage StageChunk, open OpenRange) (*Files, error) {
	if manifests == nil || chunkSize <= 0 || chunkSize > vaultmanifest.MaxChunkSize || stage == nil || open == nil {
		return nil, ErrInvalidArgument
	}
	return &Files{manifests: manifests, chunkSize: chunkSize, stage: stage, open: open}, nil
}

type sourceReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *sourceReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Build stages exact-length source chunks and an encrypted manifest. Errors
// return no manifest; the transaction owner retains every staged allocation.
// The source must remain stable while Build reads it.
func (f *Files) Build(ctx context.Context, fileID [16]byte, size int64, source io.Reader) (vaultmanifest.Manifest, error) {
	if f == nil || f.manifests == nil || fileID == ([16]byte{}) || size < 0 || source == nil {
		return vaultmanifest.Manifest{}, ErrInvalidArgument
	}
	reader := &sourceReader{ctx: ctx, r: source}
	var offset int64
	return f.manifests.Build(ctx, fileID, func(ctx context.Context) (vaultmanifest.Chunk, error) {
		if offset == size {
			var extra [1]byte
			n, err := io.ReadFull(reader, extra[:])
			if n != 0 {
				return vaultmanifest.Chunk{}, ErrSourceSize
			}
			if !errors.Is(err, io.EOF) {
				return vaultmanifest.Chunk{}, err
			}
			return vaultmanifest.Chunk{}, io.EOF
		}
		length := min(f.chunkSize, size-offset)
		limited := &io.LimitedReader{R: reader, N: length}
		chunk, err := f.stage(ctx, fileID, offset, length, limited)
		if err != nil {
			return vaultmanifest.Chunk{}, err
		}
		if err := ctx.Err(); err != nil {
			return vaultmanifest.Chunk{}, err
		}
		if limited.N != 0 {
			return vaultmanifest.Chunk{}, ErrSourceSize
		}
		if chunk.Offset != offset || chunk.Size != length {
			return vaultmanifest.Chunk{}, ErrChunk
		}
		chunk.ID = strings.Clone(chunk.ID)
		offset += length
		return chunk, nil
	})
}

// ReadAt reads authenticated file bytes at offset. A partial EOF returns io.EOF;
// data, cancellation and close errors preserve the number of bytes already read.
// The caller pins the manifest's immutable version for the whole operation.
func (f *Files) ReadAt(ctx context.Context, manifest vaultmanifest.Manifest, p []byte, offset int64) (int, error) {
	if f == nil || f.manifests == nil || offset < 0 || manifest.Size < 0 || manifest.FileID == ([16]byte{}) {
		return 0, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if offset >= manifest.Size {
		return 0, io.EOF
	}
	var done int
	for done < len(p) && offset < manifest.Size {
		chunk, err := f.manifests.Lookup(ctx, manifest, offset)
		if err != nil {
			return done, err
		}
		relative := offset - chunk.Offset
		length := min(int64(len(p)-done), chunk.Size-relative, manifest.Size-offset)
		if relative < 0 || length <= 0 {
			return done, ErrChunk
		}
		reader, err := f.open(ctx, manifest.FileID, chunk, relative, length)
		if err != nil {
			return done, err
		}
		if reader == nil {
			return done, ErrChunk
		}
		n, readErr := io.ReadFull(reader, p[done:done+int(length)])
		closeErr := reader.Close()
		done += n
		offset += int64(n)
		if err := ctx.Err(); err != nil {
			return done, err
		}
		if err := errors.Join(readErr, closeErr); err != nil {
			return done, err
		}
	}
	if done != len(p) {
		return done, io.EOF
	}
	return done, nil
}
