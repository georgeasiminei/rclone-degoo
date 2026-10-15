// Package vaultpage encrypts bounded vault metadata page values.
//
// The codec is an inactive format component. It does not create a vault root,
// contact a provider, or define publication semantics. It uses rclone crypt's
// scrypt key derivation and authenticated secretbox block stream; the stored
// page limit includes that stream's header and per-block authentication bytes.
// Callers supply immutable logical page generations and enforce publication.
package vaultpage

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
)

const (
	// MaxStoredSize caps the complete encrypted page, including crypt overhead.
	MaxStoredSize   = 128 << 10
	innerHeaderSize = 55
	innerVersion    = 2
)

var (
	pageMagic = [8]byte{'D', 'G', 'S', 'V', 'P', '0', '0', '2'}

	// ErrInvalidConfig reports invalid password, salt, or vault identity input.
	ErrInvalidConfig = errors.New("vault page: invalid codec configuration")
	// ErrInvalidIdentity reports a zero or unsupported page identity.
	ErrInvalidIdentity = errors.New("vault page: invalid page identity")
	// ErrPayloadTooLarge reports that a payload exceeds the encrypted page cap.
	ErrPayloadTooLarge = errors.New("vault page: payload exceeds stored page limit")
	// ErrDigestMismatch reports that stored bytes do not match their reference.
	ErrDigestMismatch = errors.New("vault page: encrypted page digest mismatch")
	// ErrInvalidPage reports malformed or undecryptable page bytes.
	ErrInvalidPage = errors.New("vault page: invalid encrypted page")
	// ErrIdentityMismatch reports a valid page for a different expected identity.
	ErrIdentityMismatch = errors.New("vault page: page identity mismatch")
)

// Role identifies the kind of metadata page.
type Role uint8

const (
	// RoleRoot identifies the vault root metadata page.
	RoleRoot Role = iota + 1
	// RoleIndex identifies a directory index page.
	RoleIndex
	// RoleManifest identifies a file manifest page.
	RoleManifest
	// RoleInventory identifies a transaction allocation inventory page.
	RoleInventory
)

// Identity identifies one immutable logical page generation.
type Identity struct {
	// Role determines which payload schema may consume the page.
	Role Role `json:"role"`
	// LogicalID is a nonzero immutable logical page identifier.
	LogicalID [16]byte `json:"logical_id"`
	// Generation is the expected nonzero page generation.
	Generation uint64 `json:"generation"`
}

// Codec encrypts metadata pages with rclone crypt's authenticated content cipher.
type Codec struct {
	cipher     *crypt.Cipher
	vaultID    [16]byte
	maxPayload int
}

// New constructs a codec from a non-empty password, random nonzero salt, and
// nonzero vault ID. Salt bytes are represented to crypt as lowercase hex.
func New(password string, salt [32]byte, vaultID [16]byte) (*Codec, error) {
	if password == "" || isZero(salt[:]) || isZero(vaultID[:]) {
		return nil, ErrInvalidConfig
	}
	obscuredPassword, err := obscure.Obscure(password)
	if err != nil {
		return nil, fmt.Errorf("vault page: obscure password: %w", err)
	}
	obscuredSalt, err := obscure.Obscure(hex.EncodeToString(salt[:]))
	if err != nil {
		return nil, fmt.Errorf("vault page: obscure salt: %w", err)
	}
	cipher, err := crypt.NewCipher(configmap.Simple{
		"filename_encryption": "off",
		"filename_encoding":   "base32",
		"suffix":              ".bin",
		"password":            obscuredPassword,
		"password2":           obscuredSalt,
	})
	if err != nil {
		return nil, fmt.Errorf("vault page: initialize content cipher: %w", err)
	}
	codec := &Codec{cipher: cipher, vaultID: vaultID}
	codec.maxPayload = maxPayload(cipher)
	return codec, nil
}

// MaxPayload returns the largest plaintext payload whose complete encrypted
// page fits MaxStoredSize.
func (c *Codec) MaxPayload() int {
	if c == nil {
		return 0
	}
	return c.maxPayload
}

// Seal encrypts a page and returns its stored bytes and SHA-256 reference digest.
func (c *Codec) Seal(identity Identity, payload []byte) ([]byte, [32]byte, error) {
	if c == nil || c.cipher == nil || !validIdentity(identity) {
		return nil, [32]byte{}, ErrInvalidIdentity
	}
	if len(payload) > c.maxPayload {
		return nil, [32]byte{}, ErrPayloadTooLarge
	}
	plain := encodeInner(c.vaultID, identity, payload)
	if c.cipher.EncryptedSize(int64(len(plain))) > MaxStoredSize {
		return nil, [32]byte{}, ErrPayloadTooLarge
	}
	encrypted, err := c.cipher.EncryptData(bytes.NewReader(plain))
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("vault page: encrypt page: %w", err)
	}
	stored, err := io.ReadAll(io.LimitReader(encrypted, MaxStoredSize+1))
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("vault page: read encrypted page: %w", err)
	}
	if len(stored) > MaxStoredSize || int64(len(stored)) != c.cipher.EncryptedSize(int64(len(plain))) {
		return nil, [32]byte{}, ErrPayloadTooLarge
	}
	return stored, sha256.Sum256(stored), nil
}

// Open verifies the encrypted-byte digest before decrypting, then checks the
// vault and expected logical page identity before returning the payload.
func (c *Codec) Open(expected Identity, stored []byte, digest [32]byte) ([]byte, error) {
	if c == nil || c.cipher == nil || len(stored) > MaxStoredSize {
		return nil, ErrInvalidPage
	}
	actualDigest := sha256.Sum256(stored)
	if subtle.ConstantTimeCompare(actualDigest[:], digest[:]) != 1 {
		return nil, ErrDigestMismatch
	}
	stream, err := c.cipher.DecryptData(io.NopCloser(bytes.NewReader(stored)))
	if err != nil {
		return nil, fmt.Errorf("%w: decrypt content: %v", ErrInvalidPage, err)
	}
	maxPlain := innerHeaderSize + c.maxPayload
	plain, readErr := io.ReadAll(io.LimitReader(stream, int64(maxPlain+1)))
	closeErr := stream.Close()
	if readErr != nil {
		return nil, fmt.Errorf("%w: decrypt content: %v", ErrInvalidPage, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("%w: close content: %v", ErrInvalidPage, closeErr)
	}
	if len(plain) > maxPlain || int64(len(stored)) != c.cipher.EncryptedSize(int64(len(plain))) {
		return nil, ErrInvalidPage
	}
	vaultID, identity, payload, err := decodeInner(plain, c.maxPayload)
	if err != nil {
		return nil, err
	}
	if !validIdentity(expected) {
		return nil, ErrInvalidIdentity
	}
	if vaultID != c.vaultID || identity != expected {
		return nil, ErrIdentityMismatch
	}
	return payload, nil
}

func maxPayload(cipher *crypt.Cipher) int {
	low, high := 0, MaxStoredSize
	for low <= high {
		middle := low + (high-low)/2
		if cipher.EncryptedSize(int64(innerHeaderSize+middle)) <= MaxStoredSize {
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return high
}

func encodeInner(vaultID [16]byte, identity Identity, payload []byte) []byte {
	plain := make([]byte, innerHeaderSize+len(payload))
	copy(plain[:8], pageMagic[:])
	binary.BigEndian.PutUint16(plain[8:10], innerVersion)
	plain[10] = byte(identity.Role)
	copy(plain[11:27], vaultID[:])
	copy(plain[27:43], identity.LogicalID[:])
	binary.BigEndian.PutUint64(plain[43:51], identity.Generation)
	binary.BigEndian.PutUint32(plain[51:55], uint32(len(payload)))
	copy(plain[innerHeaderSize:], payload)
	return plain
}

func decodeInner(plain []byte, maxPayload int) ([16]byte, Identity, []byte, error) {
	var vaultID [16]byte
	if len(plain) < innerHeaderSize || !bytes.Equal(plain[:8], pageMagic[:]) || binary.BigEndian.Uint16(plain[8:10]) != innerVersion {
		return vaultID, Identity{}, nil, ErrInvalidPage
	}
	identity := Identity{Role: Role(plain[10]), Generation: binary.BigEndian.Uint64(plain[43:51])}
	copy(vaultID[:], plain[11:27])
	copy(identity.LogicalID[:], plain[27:43])
	payloadLength := binary.BigEndian.Uint32(plain[51:55])
	if !validIdentity(identity) || isZero(vaultID[:]) || uint64(payloadLength) > uint64(maxPayload) || uint64(len(plain)-innerHeaderSize) != uint64(payloadLength) {
		return [16]byte{}, Identity{}, nil, ErrInvalidPage
	}
	return vaultID, identity, plain[innerHeaderSize:], nil
}

func validIdentity(identity Identity) bool {
	return identity.Role >= RoleRoot && identity.Role <= RoleInventory && identity.Generation != 0 && !isZero(identity.LogicalID[:])
}

func isZero(value []byte) bool {
	var combined byte
	for _, b := range value {
		combined |= b
	}
	return combined == 0
}
