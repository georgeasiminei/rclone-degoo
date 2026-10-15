package vaultpage

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

const testPassword = "vault-page-test-passphrase"

func testConfig() ([32]byte, [16]byte) {
	var salt [32]byte
	var vaultID [16]byte
	for i := range salt {
		salt[i] = byte(i + 1)
	}
	for i := range vaultID {
		vaultID[i] = byte(i + 17)
	}
	return salt, vaultID
}

func testIdentity() Identity {
	var logicalID [16]byte
	for i := range logicalID {
		logicalID[i] = byte(33 + i)
	}
	return Identity{Role: RoleManifest, LogicalID: logicalID, Generation: 7}
}

func newTestCodec(t *testing.T) (*Codec, [32]byte, [16]byte) {
	t.Helper()
	salt, vaultID := testConfig()
	codec, err := New(testPassword, salt, vaultID)
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	return codec, salt, vaultID
}

func TestNewRejectsZeroConfiguration(t *testing.T) {
	salt, vaultID := testConfig()
	if _, err := New("", salt, vaultID); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty password error = %v", err)
	}
	if _, err := New(testPassword, [32]byte{}, vaultID); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero salt error = %v", err)
	}
	if _, err := New(testPassword, salt, [16]byte{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero vault ID error = %v", err)
	}
}

func TestSealOpenAtStoredSizeLimit(t *testing.T) {
	codec, _, _ := newTestCodec(t)
	identity := testIdentity()
	payload := bytes.Repeat([]byte{0x5a}, codec.MaxPayload())
	stored, digest, err := codec.Seal(identity, payload)
	if err != nil {
		t.Fatal("Seal at maximum payload returned an error")
	}
	if len(stored) != MaxStoredSize {
		t.Fatalf("stored size = %d, want exact cap %d", len(stored), MaxStoredSize)
	}
	if digest != sha256.Sum256(stored) {
		t.Fatal("Seal digest does not cover complete encrypted page")
	}
	opened, err := codec.Open(identity, stored, digest)
	if err != nil || !bytes.Equal(opened, payload) {
		t.Fatalf("Open maximum payload failed (equal=%t, err=%v)", bytes.Equal(opened, payload), err)
	}
	if _, _, err := codec.Seal(identity, append(payload, 0)); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("payload above cap error = %v", err)
	}
}

func TestOpenChecksDigestBeforeDecryptAndChecksIdentity(t *testing.T) {
	codec, salt, vaultID := newTestCodec(t)
	identity := testIdentity()
	stored, digest, err := codec.Seal(identity, []byte("small payload"))
	if err != nil {
		t.Fatal("Seal returned an error")
	}

	wrongDigest := digest
	wrongDigest[0] ^= 1
	wrongPasswordCodec, err := New("different passphrase", salt, vaultID)
	if err != nil {
		t.Fatal("New for wrong-password fixture returned an error")
	}
	if _, err := wrongPasswordCodec.Open(identity, stored, wrongDigest); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("digest check did not precede decrypt: %v", err)
	}

	for name, expected := range map[string]Identity{
		"role":       func() Identity { i := identity; i.Role = RoleIndex; return i }(),
		"logical ID": func() Identity { i := identity; i.LogicalID[0] ^= 1; return i }(),
		"generation": func() Identity { i := identity; i.Generation++; return i }(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := codec.Open(expected, stored, digest); !errors.Is(err, ErrIdentityMismatch) {
				t.Fatalf("identity mismatch error = %v", err)
			}
		})
	}

	otherVault := *codec
	otherVault.vaultID[0] ^= 1
	if _, err := otherVault.Open(identity, stored, digest); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("cross-vault error = %v", err)
	}

	wrongSalt := salt
	wrongSalt[0] ^= 1
	wrongSaltCodec, err := New(testPassword, wrongSalt, vaultID)
	if err != nil {
		t.Fatal("New for wrong-salt fixture returned an error")
	}
	if _, err := wrongPasswordCodec.Open(identity, stored, digest); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("wrong-password error = %v", err)
	}
	if _, err := wrongSaltCodec.Open(identity, stored, digest); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("wrong-salt error = %v", err)
	}
}

func TestOpenRejectsTamperedAndTruncatedCiphertext(t *testing.T) {
	codec, _, _ := newTestCodec(t)
	identity := testIdentity()
	stored, _, err := codec.Seal(identity, []byte("authenticated content"))
	if err != nil {
		t.Fatal("Seal returned an error")
	}

	tampered := append([]byte(nil), stored...)
	tampered[40] ^= 1
	truncated := append([]byte(nil), stored[:len(stored)-1]...)
	for name, input := range map[string][]byte{"tampered": tampered, "truncated": truncated} {
		t.Run(name, func(t *testing.T) {
			digest := sha256.Sum256(input)
			if _, err := codec.Open(identity, input, digest); !errors.Is(err, ErrInvalidPage) {
				t.Fatalf("Open error = %v", err)
			}
		})
	}
}

func TestOpenRejectsAuthenticatedMalformedInnerHeaders(t *testing.T) {
	codec, _, vaultID := newTestCodec(t)
	identity := testIdentity()
	base := encodeInner(vaultID, identity, []byte("x"))
	mutations := map[string]func([]byte) []byte{
		"marker":          func(plain []byte) []byte { plain[0] ^= 1; return plain },
		"version":         func(plain []byte) []byte { binary.BigEndian.PutUint16(plain[8:10], innerVersion+1); return plain },
		"role":            func(plain []byte) []byte { plain[10] = 255; return plain },
		"zero vault":      func(plain []byte) []byte { clear(plain[11:27]); return plain },
		"zero logical ID": func(plain []byte) []byte { clear(plain[27:43]); return plain },
		"zero generation": func(plain []byte) []byte { clear(plain[43:51]); return plain },
		"payload length too large": func(plain []byte) []byte {
			binary.BigEndian.PutUint32(plain[51:55], uint32(codec.MaxPayload()+1))
			return plain
		},
		"payload length short": func(plain []byte) []byte { binary.BigEndian.PutUint32(plain[51:55], 0); return plain },
		"trailing bytes":       func(plain []byte) []byte { return append(plain, 0) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			plain := mutate(append([]byte(nil), base...))
			stored := encryptFixture(t, codec, plain)
			digest := sha256.Sum256(stored)
			if _, err := codec.Open(identity, stored, digest); !errors.Is(err, ErrInvalidPage) {
				t.Fatalf("Open error = %v", err)
			}
		})
	}
}

func TestOpenRejectsInvalidStoredLengthsAndIdentity(t *testing.T) {
	codec, _, _ := newTestCodec(t)
	identity := testIdentity()
	if _, err := codec.Open(identity, nil, sha256.Sum256(nil)); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("empty stored page error = %v", err)
	}
	if _, _, err := codec.Seal(Identity{Role: 255, LogicalID: identity.LogicalID, Generation: 1}, nil); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("unknown role error = %v", err)
	}
	if _, _, err := codec.Seal(Identity{Role: RoleRoot, LogicalID: identity.LogicalID}, nil); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("zero generation error = %v", err)
	}
	if _, _, err := codec.Seal(Identity{Role: RoleRoot, Generation: 1}, nil); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("zero logical ID error = %v", err)
	}
	oversized := make([]byte, MaxStoredSize+1)
	if _, err := codec.Open(identity, oversized, sha256.Sum256(oversized)); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("oversized stored page error = %v", err)
	}
}

func TestConcurrentSealAndOpen(t *testing.T) {
	codec, _, _ := newTestCodec(t)
	const workers, iterations = 8, 16
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				identity := testIdentity()
				identity.LogicalID[0] = byte(worker + 1)
				identity.Generation = uint64(i + 1)
				payload := []byte(fmt.Sprintf("worker-%d-page-%d", worker, i))
				stored, digest, err := codec.Seal(identity, payload)
				if err != nil {
					t.Errorf("Seal failed: %v", err)
					return
				}
				opened, err := codec.Open(identity, stored, digest)
				if err != nil || !bytes.Equal(opened, payload) {
					t.Errorf("Open failed (equal=%t, err=%v)", bytes.Equal(opened, payload), err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}

func encryptFixture(t *testing.T, codec *Codec, plain []byte) []byte {
	t.Helper()
	reader, err := codec.cipher.EncryptData(bytes.NewReader(plain))
	if err != nil {
		t.Fatal("encrypt fixture setup failed")
	}
	stored, err := io.ReadAll(io.LimitReader(reader, MaxStoredSize+1))
	if err != nil || len(stored) > MaxStoredSize {
		t.Fatalf("encrypt fixture failed (size=%d, err=%v)", len(stored), err)
	}
	return stored
}
