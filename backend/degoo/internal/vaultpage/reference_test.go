package vaultpage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type repositoryStore struct {
	mu             sync.Mutex
	objects        map[string][]byte
	gets, puts     int
	putErr, getErr error
	locator        string
}

func (s *repositoryStore) Get(_ context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getErr != nil {
		return nil, s.getErr
	}
	b, ok := s.objects[id]
	if !ok {
		return nil, errors.New("generated page missing")
	}
	return bytes.Clone(b), nil
}
func (s *repositoryStore) Put(_ context.Context, _ Identity, b []byte, digest [32]byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if s.putErr != nil {
		return "", s.putErr
	}
	if sha256.Sum256(b) != digest {
		return "", errors.New("wrong stored digest")
	}
	id := s.locator
	if id == "" {
		id = fmt.Sprintf("page-%d", s.puts)
	}
	if s.objects == nil {
		s.objects = make(map[string][]byte)
	}
	s.objects[id] = bytes.Clone(b)
	return id, nil
}
func testRepository(t *testing.T) (*Repository, *repositoryStore) {
	t.Helper()
	codec, _, _ := newTestCodec(t)
	store := new(repositoryStore)
	repo, err := NewRepository(codec, store)
	if err != nil {
		t.Fatal(err)
	}
	return repo, store
}
func TestRepositoryRoundTripAndReferenceAdmission(t *testing.T) {
	repo, store := testRepository(t)
	ctx := context.Background()
	id := testIdentity()
	ref, err := repo.Put(ctx, id, []byte("generated metadata"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.Get(ctx, ref)
	if err != nil || string(b) != "generated metadata" {
		t.Fatalf("roundtrip=%q,%v", b, err)
	}
	for _, bad := range []Reference{{}, {Locator: "x", Identity: id}, {Locator: "\x00", Identity: id, Digest: ref.Digest}, {Locator: strings.Repeat("x", MaxLocatorSize+1), Identity: id, Digest: ref.Digest}, {Locator: string([]byte{0xff}), Identity: id, Digest: ref.Digest}} {
		before := store.gets
		if _, err := repo.Get(ctx, bad); !errors.Is(err, ErrInvalidReference) {
			t.Fatalf("bad reference error=%v", err)
		}
		if store.gets != before {
			t.Fatal("invalid reference reached store")
		}
	}
	if err := ref.Validate(RoleIndex); !errors.Is(err, ErrInvalidReference) {
		t.Fatal("accepted wrong role")
	}
	if repo.MaxPayload() != repo.codec.MaxPayload() {
		t.Fatal("wrong payload budget")
	}
}
func TestRepositorySingleAttemptAndCancellation(t *testing.T) {
	repo, store := testRepository(t)
	failure := errors.New("unknown create outcome")
	store.putErr = failure
	if _, err := repo.Put(context.Background(), testIdentity(), nil); !errors.Is(err, failure) || store.puts != 1 {
		t.Fatalf("put=%v calls=%d", err, store.puts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.Put(ctx, testIdentity(), nil); !errors.Is(err, context.Canceled) || store.puts != 1 {
		t.Fatal("canceled create reached store")
	}
	if _, err := repo.Get(ctx, Reference{}); !errors.Is(err, context.Canceled) || store.gets != 0 {
		t.Fatal("canceled read reached store")
	}
	store.putErr = nil
	ref, err := repo.Put(context.Background(), testIdentity(), nil)
	if err != nil {
		t.Fatal(err)
	}
	store.getErr = failure
	if _, err := repo.Get(context.Background(), ref); !errors.Is(err, failure) || store.gets != 1 {
		t.Fatal("read failure replayed or hidden")
	}
}
func TestRepositoryRejectsSubstitutionAndOversizedResponse(t *testing.T) {
	repo, store := testRepository(t)
	ctx := context.Background()
	identity := testIdentity()
	ref, err := repo.Put(ctx, identity, []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := repo.Put(ctx, identity, []byte("replacement"))
	if err != nil {
		t.Fatal(err)
	}
	store.objects[ref.Locator] = bytes.Clone(store.objects[other.Locator])
	if _, err := repo.Get(ctx, ref); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("substitution error=%v", err)
	}
	store.objects[ref.Locator] = make([]byte, MaxStoredSize+1)
	if _, err := repo.Get(ctx, ref); !errors.Is(err, ErrInvalidPage) {
		t.Fatalf("oversized read error=%v", err)
	}
	store.locator = "\x00"
	if _, err := repo.Put(ctx, identity, nil); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("invalid returned locator=%v", err)
	}
	before := store.puts
	if _, err := repo.Put(ctx, identity, make([]byte, repo.MaxPayload()+1)); !errors.Is(err, ErrPayloadTooLarge) || store.puts != before {
		t.Fatal("oversized payload reached store")
	}
}
func TestRepositoryRejectsUninitializedDependencies(t *testing.T) {
	codec, _, _ := newTestCodec(t)
	for _, args := range []struct {
		codec *Codec
		store Store
	}{{nil, new(repositoryStore)}, {new(Codec), new(repositoryStore)}, {codec, nil}} {
		if _, err := NewRepository(args.codec, args.store); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("accepted invalid repository dependencies")
		}
	}
}
