package vaultpage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxLocatorSize caps the byte length of an opaque physical page locator.
const MaxLocatorSize = 256

// ErrInvalidReference reports an incomplete, oversized or wrong-role reference.
var ErrInvalidReference = errors.New("vault page: invalid page reference")

// Reference identifies immutable encrypted bytes and their expected logical identity.
// Its digest and identity must originate in trusted control state or an
// authenticated parent page, never in an untrusted provider listing.
type Reference struct {
	// Locator is the bounded opaque immutable-object address.
	Locator string `json:"locator"`
	// Identity is the expected authenticated page identity.
	Identity Identity `json:"identity"`
	// Digest commits to the complete stored ciphertext.
	Digest [32]byte `json:"digest"`
}

// Validate checks the reference and requires the caller's expected page role.
func (r Reference) Validate(role Role) error {
	if !validIdentity(r.Identity) || r.Identity.Role != role || !validLocator(r.Locator) || isZero(r.Digest[:]) {
		return ErrInvalidReference
	}
	return nil
}

func validLocator(locator string) bool {
	return locator != "" && len(locator) <= MaxLocatorSize && utf8.ValidString(locator) && !strings.ContainsRune(locator, '\x00')
}

// Store stages and reads immutable encrypted pages. Implementations must persist
// allocation intent before a create, retain unknown outcomes, honor cancellation
// and shared provider/auth cooldowns, and never overwrite or reuse a locator.
// Put returns success only after confirming stored bytes against digest. Failed
// creates remain in the transaction inventory, even without a known locator.
// Get must bound its response to MaxStoredSize before reading it into memory.
// These are adapter requirements, not verified Degoo capabilities.
type Store interface {
	// Get returns the complete exact object or rejects an object over MaxStoredSize;
	// a bounded read must detect overflow rather than accept a truncated prefix.
	Get(ctx context.Context, locator string) ([]byte, error)
	// Put stages one immutable create after persisting its allocation intent.
	Put(ctx context.Context, identity Identity, stored []byte, digest [32]byte) (locator string, err error)
}

// Repository verifies encrypted page references and stages pages through a store.
// It does not retry requests, clean staged objects, publish roots or provide
// freshness, allocation intent, transaction ownership or provider fencing.
type Repository struct {
	codec *Codec
	store Store
}

// NewRepository binds an initialized codec and a non-nil store.
func NewRepository(codec *Codec, store Store) (*Repository, error) {
	if codec == nil || codec.cipher == nil || store == nil {
		return nil, ErrInvalidConfig
	}
	return &Repository{codec: codec, store: store}, nil
}

// MaxPayload returns the maximum encoded payload accepted by this repository.
func (r *Repository) MaxPayload() int {
	if r == nil || r.codec == nil {
		return 0
	}
	return r.codec.MaxPayload()
}

// Put seals payload and sends one immutable create to the store. On failure the
// store retains allocation bookkeeping; no retry or cleanup is attempted here.
func (r *Repository) Put(ctx context.Context, identity Identity, payload []byte) (Reference, error) {
	if err := ctx.Err(); err != nil {
		return Reference{}, err
	}
	if r == nil || r.codec == nil || r.store == nil {
		return Reference{}, ErrInvalidConfig
	}
	stored, digest, err := r.codec.Seal(identity, payload)
	if err != nil {
		return Reference{}, err
	}
	if err := ctx.Err(); err != nil {
		return Reference{}, err
	}
	locator, err := r.store.Put(ctx, identity, stored, digest)
	if err != nil {
		return Reference{}, fmt.Errorf("vault page: stage page: %w", err)
	}
	ref := Reference{Locator: strings.Clone(locator), Identity: identity, Digest: digest}
	if err := ref.Validate(identity.Role); err != nil {
		return Reference{}, err
	}
	return ref, nil
}

// Get validates a trusted reference and authenticates stored bytes before
// returning payload. It performs at most one store read.
func (r *Repository) Get(ctx context.Context, ref Reference) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.codec == nil || r.store == nil {
		return nil, ErrInvalidConfig
	}
	if err := ref.Validate(ref.Identity.Role); err != nil {
		return nil, err
	}
	stored, err := r.store.Get(ctx, ref.Locator)
	if err != nil {
		return nil, fmt.Errorf("vault page: read page: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.codec.Open(ref.Identity, stored, ref.Digest)
}
