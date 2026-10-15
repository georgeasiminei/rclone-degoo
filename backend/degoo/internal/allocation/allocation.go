// Package allocation persists bounded local intent before immutable object
// creates. It does not issue provider requests, replay attempts, or publish
// roots; callers keep the authority lease open for the ledger's lifetime.
package allocation

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// MaxRecords bounds unresolved and confirmed allocations retained locally.
	MaxRecords = 32
	// MaxCheckpointSize keeps the encoded ledger below the authority envelope cap.
	MaxCheckpointSize = 60 << 10
	// MaxMetadataStoredLength bounds encrypted metadata page objects.
	MaxMetadataStoredLength uint64 = 128 << 10
	// MaxDataStoredLength includes the supported maximum logical chunk and framing.
	MaxDataStoredLength uint64 = (1 << 30) + (1 << 20)
	// MaxLocatorLength bounds a confirmed immutable provider locator in bytes.
	MaxLocatorLength = 256
	stateVersion     = 1
)

var (
	// ErrInvalidArgument reports invalid checkpoint or reservation input.
	ErrInvalidArgument = errors.New("allocation ledger: invalid argument")
	// ErrIncompatibleCheckpoint reports a checkpoint for another vault or version.
	ErrIncompatibleCheckpoint = errors.New("allocation ledger: incompatible checkpoint")
	// ErrCorruptCheckpoint reports malformed, inconsistent, or oversized ledger state.
	ErrCorruptCheckpoint = errors.New("allocation ledger: corrupt checkpoint")
	// ErrCheckpointTooLarge reports encoded ledger state over MaxCheckpointSize.
	ErrCheckpointTooLarge = errors.New("allocation ledger: checkpoint exceeds size limit")
	// ErrCheckpointFailure reports a checkpoint port failure.
	ErrCheckpointFailure = errors.New("allocation ledger: checkpoint operation failed")
	// ErrPoisoned reports that a checkpoint failure made this instance unusable.
	ErrPoisoned = errors.New("allocation ledger: instance is poisoned")
	// ErrStaleLedger reports another instance's newer checkpoint state.
	ErrStaleLedger = errors.New("allocation ledger: checkpoint state advanced")
	// ErrCapacity reports that all bounded allocation slots are retained.
	ErrCapacity = errors.New("allocation ledger: allocation capacity exhausted")
	// ErrSequenceExhausted reports that the persisted attempt sequence cannot advance.
	ErrSequenceExhausted = errors.New("allocation ledger: attempt sequence exhausted")
	// ErrDuplicateIdentity reports an identity and offset already reserved.
	ErrDuplicateIdentity = errors.New("allocation ledger: identity already reserved")
	// ErrUnknownAttempt reports a token that is absent from this ledger.
	ErrUnknownAttempt = errors.New("allocation ledger: unknown attempt")
	// ErrConfirmationConflict reports confirmation data that conflicts with intent.
	ErrConfirmationConflict = errors.New("allocation ledger: confirmation conflicts with reservation")
	// ErrInvalidLocator reports an empty, oversized, malformed or NUL-containing locator.
	ErrInvalidLocator = errors.New("allocation ledger: invalid locator")
)

// Checkpoint is the durable local state port required by a Ledger. The lease
// must remain held for the ledger lifetime; methods must fail closed on I/O
// ambiguity and must not silently initialize missing state during Load.
// CompareAndSaveCheckpoint atomically refuses a mismatching expected payload
// and returns success only after the replacement is durable.
type Checkpoint interface {
	// Check verifies the retained writer authority and loaded state.
	Check() error
	// InitializeCheckpoint explicitly creates absent state without replacement.
	InitializeCheckpoint([]byte) error
	// LoadCheckpoint reads existing state without recreating it.
	LoadCheckpoint() ([]byte, error)
	// CompareAndSaveCheckpoint durably replaces an exact expected local payload.
	CompareAndSaveCheckpoint(expected, payload []byte) error
}

// Kind identifies the stored object class and its maximum permitted size.
type Kind uint8

const (
	// MetadataPage identifies an encrypted metadata page.
	MetadataPage Kind = iota + 1
	// DataChunk identifies an encrypted file-data chunk.
	DataChunk
)

// Request is the immutable identity and byte commitment reserved before a
// caller may issue one create. Offset is zero for metadata pages and identifies
// a chunk position for data objects.
type Request struct {
	// Kind selects the stored-object size bound.
	Kind Kind
	// LogicalID is the allocation's unique logical object identity.
	LogicalID [16]byte
	// Offset identifies a data position, or zero for metadata.
	Offset int64
	// ExpectedStoredLength commits to the complete encrypted object size.
	ExpectedStoredLength uint64
	// Digest commits to the complete stored ciphertext.
	Digest [32]byte
}

// Token identifies one persisted create attempt. A token is returned only
// after its reservation has been committed to the checkpoint.
type Token struct {
	// Sequence is a persisted monotonically increasing attempt number.
	Sequence uint64
	// AttemptID is a nonzero random attempt identity.
	AttemptID [16]byte
}

// Record is a copy of one retained allocation reservation and optional exact
// object confirmation. Returned records do not expose the ledger's live state.
type Record struct {
	// Token identifies the persisted create attempt.
	Token Token
	// Request contains the pre-create byte commitment.
	Request Request
	// Confirmed records positive verification of the exact object bytes.
	Confirmed bool
	// Locator is empty until exact-object confirmation succeeds.
	Locator string
}

type checkpointState struct {
	Version      int      `json:"version"`
	VaultID      [16]byte `json:"vault_id"`
	LastSequence uint64   `json:"last_sequence"`
	Records      []Record `json:"records"`
}

// Ledger serializes bounded intent updates over a durable checkpoint port.
// It never owns or closes the caller's authority lease.
// Conditional checkpoint replacement refuses stale independent ledger instances.
type Ledger struct {
	mu         sync.Mutex
	checkpoint Checkpoint
	state      checkpointState
	poison     error
}

// Initialize explicitly creates the first ledger checkpoint and returns a
// ready ledger. Existing checkpoint state is never replaced.
func Initialize(checkpoint Checkpoint, vaultID [16]byte) (*Ledger, error) {
	if checkpoint == nil || zero(vaultID[:]) {
		return nil, ErrInvalidArgument
	}
	if err := checkpoint.Check(); err != nil {
		return nil, checkpointError(err)
	}
	state := checkpointState{Version: stateVersion, VaultID: vaultID, Records: []Record{}}
	payload, err := encodeState(state)
	if err != nil {
		return nil, err
	}
	if err := checkpoint.InitializeCheckpoint(payload); err != nil {
		return nil, checkpointError(err)
	}
	return &Ledger{checkpoint: checkpoint, state: state}, nil
}

// Open loads an existing compatible ledger. Missing or corrupt checkpoints are
// returned as errors; Open never creates an empty ledger.
func Open(checkpoint Checkpoint, vaultID [16]byte) (*Ledger, error) {
	if checkpoint == nil || zero(vaultID[:]) {
		return nil, ErrInvalidArgument
	}
	if err := checkpoint.Check(); err != nil {
		return nil, checkpointError(err)
	}
	payload, err := checkpoint.LoadCheckpoint()
	if err != nil {
		return nil, checkpointError(err)
	}
	state, err := decodeState(payload)
	if err != nil {
		return nil, err
	}
	if state.Version != stateVersion || state.VaultID != vaultID {
		return nil, ErrIncompatibleCheckpoint
	}
	return &Ledger{checkpoint: checkpoint, state: state}, nil
}

// Check verifies the caller-owned lease before ledger work. Any checkpoint
// error poisons this instance until a new one is opened from committed state.
func (l *Ledger) Check() error {
	if l == nil {
		return ErrInvalidArgument
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checkLocked()
}

// Reserve persists request before returning a token that may authorize one
// provider create. A checkpoint error returns a zero token and poisons the
// ledger; reopening preserves any reservation that may have committed.
func (l *Ledger) Reserve(request Request) (Token, error) {
	if l == nil {
		return Token{}, ErrInvalidArgument
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkLocked(); err != nil {
		return Token{}, err
	}
	if err := validateRequest(request); err != nil {
		return Token{}, err
	}
	if len(l.state.Records) >= MaxRecords {
		return Token{}, ErrCapacity
	}
	if l.state.LastSequence == math.MaxUint64 {
		return Token{}, ErrSequenceExhausted
	}
	for _, record := range l.state.Records {
		if sameIdentity(record.Request, request) {
			return Token{}, ErrDuplicateIdentity
		}
	}
	token, err := l.newTokenLocked(l.state.LastSequence + 1)
	if err != nil {
		return Token{}, err
	}
	candidate := cloneState(l.state)
	candidate.LastSequence = token.Sequence
	candidate.Records = append(candidate.Records, Record{Token: token, Request: request})
	if err := l.saveLocked(candidate); err != nil {
		return Token{}, err
	}
	l.state = candidate
	return token, nil
}

// Confirm records an exact-object confirmation only when caller-verified bytes
// match the reserved stored length and digest. Repeating the exact confirmation
// is idempotent; a conflict never changes the checkpoint.
func (l *Ledger) Confirm(token Token, locator string, verifiedStoredLength uint64, verifiedDigest [32]byte) (Record, error) {
	if l == nil {
		return Record{}, ErrInvalidArgument
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkLocked(); err != nil {
		return Record{}, err
	}
	if !validLocator(locator) {
		return Record{}, ErrInvalidLocator
	}
	index := -1
	for i := range l.state.Records {
		if l.state.Records[i].Token == token {
			index = i
			break
		}
	}
	if index < 0 || token.Sequence == 0 || zero(token.AttemptID[:]) {
		return Record{}, ErrUnknownAttempt
	}
	current := l.state.Records[index]
	if current.Request.ExpectedStoredLength != verifiedStoredLength || current.Request.Digest != verifiedDigest {
		return Record{}, ErrConfirmationConflict
	}
	if current.Confirmed {
		if current.Locator != locator {
			return Record{}, ErrConfirmationConflict
		}
		return cloneRecord(current), nil
	}
	for i, record := range l.state.Records {
		if i != index && record.Confirmed && record.Locator == locator {
			return Record{}, ErrConfirmationConflict
		}
	}
	candidate := cloneState(l.state)
	candidate.Records[index].Confirmed = true
	candidate.Records[index].Locator = strings.Clone(locator)
	if err := l.saveLocked(candidate); err != nil {
		return Record{}, err
	}
	l.state = candidate
	return cloneRecord(candidate.Records[index]), nil
}

// Records returns independent copies of all unresolved and confirmed records.
func (l *Ledger) Records() ([]Record, error) {
	if l == nil {
		return nil, ErrInvalidArgument
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkLocked(); err != nil {
		return nil, err
	}
	return cloneRecords(l.state.Records), nil
}

func (l *Ledger) checkLocked() error {
	if l == nil || l.checkpoint == nil {
		return ErrInvalidArgument
	}
	if l.poison != nil {
		return fmt.Errorf("%w: %w", ErrPoisoned, l.poison)
	}
	err := l.checkpoint.Check()
	if err != nil {
		l.poison = checkpointError(err)
		return l.poison
	}
	payload, err := l.checkpoint.LoadCheckpoint()
	if err == nil {
		var expected []byte
		expected, err = encodeState(l.state)
		if err == nil && !bytes.Equal(payload, expected) {
			err = ErrStaleLedger
		}
	}
	if err != nil {
		l.poison = checkpointError(err)
		return l.poison
	}
	return nil
}

func (l *Ledger) saveLocked(candidate checkpointState) error {
	payload, err := encodeState(candidate)
	if err != nil {
		return err
	}
	expected, err := encodeState(l.state)
	if err != nil {
		return err
	}
	if err := l.checkpoint.CompareAndSaveCheckpoint(expected, payload); err != nil {
		l.poison = checkpointError(err)
		return l.poison
	}
	return nil
}

func (l *Ledger) newTokenLocked(sequence uint64) (Token, error) {
	for range 8 {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return Token{}, fmt.Errorf("allocation ledger: generate attempt identity: %w", err)
		}
		if zero(id[:]) {
			continue
		}
		unique := true
		for _, record := range l.state.Records {
			if record.Token.AttemptID == id {
				unique = false
				break
			}
		}
		if unique {
			return Token{Sequence: sequence, AttemptID: id}, nil
		}
	}
	return Token{}, errors.New("allocation ledger: random attempt identity collision")
}

func validateRequest(request Request) error {
	if zero(request.LogicalID[:]) || zero(request.Digest[:]) || request.Offset < 0 {
		return ErrInvalidArgument
	}
	var maximum uint64
	switch request.Kind {
	case MetadataPage:
		maximum = MaxMetadataStoredLength
		if request.Offset != 0 {
			return ErrInvalidArgument
		}
	case DataChunk:
		maximum = MaxDataStoredLength
	default:
		return ErrInvalidArgument
	}
	if request.ExpectedStoredLength == 0 || request.ExpectedStoredLength > maximum {
		return ErrInvalidArgument
	}
	return nil
}

func validLocator(locator string) bool {
	return locator != "" && len(locator) <= MaxLocatorLength && utf8.ValidString(locator) && !bytes.ContainsRune([]byte(locator), '\x00')
}

func sameIdentity(a, b Request) bool {
	return a.Kind == b.Kind && a.LogicalID == b.LogicalID && a.Offset == b.Offset
}

func checkpointError(err error) error {
	return fmt.Errorf("%w: %w", ErrCheckpointFailure, err)
}

func cloneState(state checkpointState) checkpointState {
	state.Records = cloneRecords(state.Records)
	return state
}

func cloneRecords(records []Record) []Record {
	result := make([]Record, len(records))
	for i, record := range records {
		result[i] = cloneRecord(record)
	}
	return result
}

func cloneRecord(record Record) Record {
	return record
}

func encodeState(state checkpointState) ([]byte, error) {
	if err := validateState(state); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("allocation ledger: encode checkpoint: %w", err)
	}
	if len(encoded) > MaxCheckpointSize {
		return nil, ErrCheckpointTooLarge
	}
	return encoded, nil
}

func decodeState(encoded []byte) (checkpointState, error) {
	if len(encoded) > MaxCheckpointSize {
		return checkpointState{}, ErrCheckpointTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var state checkpointState
	if err := decoder.Decode(&state); err != nil {
		return checkpointState{}, fmt.Errorf("%w: decode: %v", ErrCorruptCheckpoint, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return checkpointState{}, ErrCorruptCheckpoint
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return checkpointState{}, ErrCorruptCheckpoint
	}
	if state.Version != stateVersion {
		return checkpointState{}, ErrIncompatibleCheckpoint
	}
	if err := validateState(state); err != nil {
		return checkpointState{}, err
	}
	return state, nil
}

func validateState(state checkpointState) error {
	if state.Version != stateVersion || zero(state.VaultID[:]) || state.Records == nil || len(state.Records) > MaxRecords {
		return ErrCorruptCheckpoint
	}
	var previous uint64
	tokens := make(map[[16]byte]struct{}, len(state.Records))
	locators := make(map[string]struct{}, len(state.Records))
	identities := make(map[Request]struct{}, len(state.Records))
	for _, record := range state.Records {
		if record.Token.Sequence == 0 || record.Token.Sequence <= previous || record.Token.Sequence > state.LastSequence || zero(record.Token.AttemptID[:]) || validateRequest(record.Request) != nil {
			return ErrCorruptCheckpoint
		}
		if _, exists := tokens[record.Token.AttemptID]; exists {
			return ErrCorruptCheckpoint
		}
		tokens[record.Token.AttemptID] = struct{}{}
		identity := record.Request
		identity.ExpectedStoredLength = 0
		identity.Digest = [32]byte{}
		if _, exists := identities[identity]; exists {
			return ErrCorruptCheckpoint
		}
		identities[identity] = struct{}{}
		if record.Confirmed {
			if !validLocator(record.Locator) {
				return ErrCorruptCheckpoint
			}
			if _, exists := locators[record.Locator]; exists {
				return ErrCorruptCheckpoint
			}
			locators[record.Locator] = struct{}{}
		} else if record.Locator != "" {
			return ErrCorruptCheckpoint
		}
		previous = record.Token.Sequence
	}
	if state.LastSequence < previous {
		return ErrCorruptCheckpoint
	}
	return nil
}

func zero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}
