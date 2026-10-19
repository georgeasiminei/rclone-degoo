package allocation

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
)

var (
	errFakeMissing = errors.New("fake checkpoint is missing")
	errFakeExists  = errors.New("fake checkpoint already exists")
	errSaveBefore  = errors.New("injected save failure before commit")
	errSaveAfter   = errors.New("injected save failure after commit")
)

type fakeCheckpoint struct {
	mu          sync.Mutex
	payload     []byte
	initialized bool
	loaded      bool
	checkErr    error
	loadErr     error
	initErr     error
	saveBefore  error
	saveAfter   error
	checks      int
	loads       int
	initializes int
	saves       int
}

func (c *fakeCheckpoint) Check() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks++
	return c.checkErr
}

func (c *fakeCheckpoint) InitializeCheckpoint(payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initializes++
	if c.initErr != nil {
		return c.initErr
	}
	if c.initialized {
		return errFakeExists
	}
	c.payload = bytes.Clone(payload)
	c.initialized = true
	c.loaded = true
	return nil
}

func (c *fakeCheckpoint) LoadCheckpoint() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loads++
	if c.loadErr != nil {
		return nil, c.loadErr
	}
	if !c.initialized {
		return nil, errFakeMissing
	}
	c.loaded = true
	return bytes.Clone(c.payload), nil
}

func (c *fakeCheckpoint) SaveCheckpoint(payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked(payload)
}

func (c *fakeCheckpoint) CompareAndSaveCheckpoint(expected, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !bytes.Equal(c.payload, expected) {
		return errors.New("stale checkpoint payload")
	}
	return c.saveLocked(payload)
}

func (c *fakeCheckpoint) saveLocked(payload []byte) error {
	c.saves++
	if !c.loaded {
		return errors.New("fake save before positive load or initialize")
	}
	if c.saveBefore != nil {
		return c.saveBefore
	}
	c.payload = bytes.Clone(payload)
	if c.saveAfter != nil {
		return c.saveAfter
	}
	return nil
}

func (c *fakeCheckpoint) snapshot() ([]byte, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.payload), c.saves
}

func allocationVaultID() (id [16]byte) {
	for i := range id {
		id[i] = byte(i + 17)
	}
	return id
}

func allocationRequest(n uint64) Request {
	var logicalID [16]byte
	binary.BigEndian.PutUint64(logicalID[8:], n+1)
	digest := sha256.Sum256([]byte(fmt.Sprintf("allocation-%d", n)))
	return Request{Kind: DataChunk, LogicalID: logicalID, Offset: int64(n), ExpectedStoredLength: 1024, Digest: digest}
}

func initializedLedger(t *testing.T) (*Ledger, *fakeCheckpoint) {
	t.Helper()
	checkpoint := &fakeCheckpoint{}
	ledger, err := Initialize(checkpoint, allocationVaultID())
	if err != nil {
		t.Fatalf("initialize allocation ledger: %v", err)
	}
	return ledger, checkpoint
}

func TestInitializeOpenAndExplicitMissingState(t *testing.T) {
	checkpoint := &fakeCheckpoint{}
	ledger, err := Initialize(checkpoint, allocationVaultID())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if checkpoint.initializes != 1 || checkpoint.loads != 0 {
		t.Fatalf("Initialize calls: init=%d load=%d", checkpoint.initializes, checkpoint.loads)
	}
	if _, err := Initialize(checkpoint, allocationVaultID()); !errors.Is(err, errFakeExists) || !errors.Is(err, ErrCheckpointFailure) {
		t.Fatalf("second Initialize = %v, want checkpoint exists", err)
	}
	opened, err := Open(checkpoint, allocationVaultID())
	if err != nil {
		t.Fatalf("Open initialized ledger: %v", err)
	}
	if records, err := opened.Records(); err != nil || len(records) != 0 {
		t.Fatalf("opened records = %#v, %v", records, err)
	}

	missing := &fakeCheckpoint{}
	if _, err := Open(missing, allocationVaultID()); !errors.Is(err, errFakeMissing) {
		t.Fatalf("Open missing checkpoint = %v", err)
	}
	if missing.initializes != 0 {
		t.Fatal("Open initialized a missing checkpoint")
	}
	if err := ledger.Check(); err != nil {
		t.Fatalf("Check initialized ledger: %v", err)
	}
}

func TestReservePersistsBeforeReturningAndConfirmIsExactAndIdempotent(t *testing.T) {
	ledger, checkpoint := initializedLedger(t)
	request := allocationRequest(1)
	token, err := ledger.Reserve(request)
	if err != nil || token.Sequence != 1 || token.AttemptID == ([16]byte{}) {
		t.Fatalf("Reserve token/error = %#v/%v", token, err)
	}
	payload, saves := checkpoint.snapshot()
	state, err := decodeState(payload)
	if err != nil || saves != 1 || len(state.Records) != 1 || state.Records[0].Token != token || state.Records[0].Request != request || state.Records[0].Confirmed {
		t.Fatalf("persisted reservation = %#v, saves=%d, error=%v", state, saves, err)
	}

	if _, err := ledger.Confirm(token, "immutable/object-1", request.ExpectedStoredLength-1, request.Digest); !errors.Is(err, ErrConfirmationConflict) {
		t.Fatalf("wrong confirmation length = %v", err)
	}
	wrongDigest := sha256.Sum256([]byte("not the reserved bytes"))
	if _, err := ledger.Confirm(token, "immutable/object-1", request.ExpectedStoredLength, wrongDigest); !errors.Is(err, ErrConfirmationConflict) {
		t.Fatalf("wrong confirmation digest = %v", err)
	}
	confirmed, err := ledger.Confirm(token, "immutable/object-1", request.ExpectedStoredLength, request.Digest)
	if err != nil || !confirmed.Confirmed || confirmed.Locator != "immutable/object-1" || confirmed.Request != request {
		t.Fatalf("Confirm = %#v, %v", confirmed, err)
	}
	_, savesAfterCommit := checkpoint.snapshot()
	repeated, err := ledger.Confirm(token, "immutable/object-1", request.ExpectedStoredLength, request.Digest)
	if err != nil || repeated != confirmed {
		t.Fatalf("repeat exact confirmation = %#v, %v", repeated, err)
	}
	_, savesAfterRepeat := checkpoint.snapshot()
	if savesAfterRepeat != savesAfterCommit {
		t.Fatalf("idempotent confirmation wrote checkpoint: saves %d -> %d", savesAfterCommit, savesAfterRepeat)
	}
	if _, err := ledger.Confirm(token, "different/object", request.ExpectedStoredLength, request.Digest); !errors.Is(err, ErrConfirmationConflict) {
		t.Fatalf("conflicting locator confirmation = %v", err)
	}
	if _, err := ledger.Confirm(Token{}, "immutable/object-1", request.ExpectedStoredLength, request.Digest); !errors.Is(err, ErrUnknownAttempt) {
		t.Fatalf("unknown confirmation = %v", err)
	}
}

func TestCheckpointFailurePoisonsAndNeverReturnsUncommittedToken(t *testing.T) {
	t.Run("failure before commit", func(t *testing.T) {
		ledger, checkpoint := initializedLedger(t)
		checkpoint.mu.Lock()
		checkpoint.saveBefore = errSaveBefore
		checkpoint.mu.Unlock()
		token, err := ledger.Reserve(allocationRequest(2))
		if token != (Token{}) || !errors.Is(err, ErrCheckpointFailure) || !errors.Is(err, errSaveBefore) {
			t.Fatalf("failed Reserve token/error = %#v/%v", token, err)
		}
		if _, err := ledger.Records(); !errors.Is(err, ErrPoisoned) {
			t.Fatalf("Records after checkpoint failure = %v, want poisoned", err)
		}
		payload, _ := checkpoint.snapshot()
		state, decodeErr := decodeState(payload)
		if decodeErr != nil || len(state.Records) != 0 {
			t.Fatalf("pre-commit failure changed state: %#v, %v", state, decodeErr)
		}
	})

	t.Run("failure after commit", func(t *testing.T) {
		ledger, checkpoint := initializedLedger(t)
		checkpoint.mu.Lock()
		checkpoint.saveAfter = errSaveAfter
		checkpoint.mu.Unlock()
		token, err := ledger.Reserve(allocationRequest(3))
		if token != (Token{}) || !errors.Is(err, ErrCheckpointFailure) || !errors.Is(err, errSaveAfter) {
			t.Fatalf("ambiguous Reserve token/error = %#v/%v", token, err)
		}
		if err := ledger.Check(); !errors.Is(err, ErrPoisoned) {
			t.Fatalf("Check after ambiguous commit = %v", err)
		}
		checkpoint.mu.Lock()
		checkpoint.saveAfter = nil
		checkpoint.mu.Unlock()
		reopened, err := Open(checkpoint, allocationVaultID())
		if err != nil {
			t.Fatalf("reopen after ambiguous commit: %v", err)
		}
		records, err := reopened.Records()
		if err != nil || len(records) != 1 || records[0].Confirmed || records[0].Request != allocationRequest(3) {
			t.Fatalf("reopened unknown reservation = %#v, %v", records, err)
		}
	})
}

func TestOpenRejectsCorruptOversizedAndIncompatibleCheckpoints(t *testing.T) {
	vaultID := allocationVaultID()
	cases := []struct {
		name    string
		payload []byte
		want    error
	}{
		{name: "malformed", payload: []byte("{"), want: ErrCorruptCheckpoint},
		{name: "unknown field", payload: []byte(`{"version":1,"vault_id":[17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32],"last_sequence":0,"records":[],"extra":true}`), want: ErrCorruptCheckpoint},
		{name: "oversized", payload: bytes.Repeat([]byte{'x'}, MaxCheckpointSize+1), want: ErrCheckpointTooLarge},
		{name: "version", payload: mustJSON(t, checkpointState{Version: stateVersion + 1, VaultID: vaultID, Records: []Record{}}), want: ErrIncompatibleCheckpoint},
		{name: "vault identity", payload: mustJSON(t, checkpointState{Version: stateVersion, VaultID: [16]byte{99}, Records: []Record{}}), want: ErrIncompatibleCheckpoint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkpoint := &fakeCheckpoint{initialized: true, loaded: false, payload: bytes.Clone(tc.payload)}
			if _, err := Open(checkpoint, vaultID); !errors.Is(err, tc.want) {
				t.Fatalf("Open error = %v, want %v", err, tc.want)
			}
			if checkpoint.initializes != 0 {
				t.Fatal("Open initialized an invalid checkpoint")
			}
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestReserveRefusesInvalidDuplicateCapacityAndSequenceOverflow(t *testing.T) {
	ledger, _ := initializedLedger(t)
	valid := allocationRequest(10)
	invalid := []Request{
		{},
		{Kind: DataChunk, LogicalID: valid.LogicalID, Offset: -1, ExpectedStoredLength: 1, Digest: valid.Digest},
		{Kind: DataChunk, LogicalID: valid.LogicalID, ExpectedStoredLength: MaxDataStoredLength + 1, Digest: valid.Digest},
		{Kind: MetadataPage, LogicalID: valid.LogicalID, Offset: 1, ExpectedStoredLength: 1, Digest: valid.Digest},
		{Kind: 99, LogicalID: valid.LogicalID, ExpectedStoredLength: 1, Digest: valid.Digest},
	}
	for _, request := range invalid {
		if token, err := ledger.Reserve(request); token != (Token{}) || !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("Reserve invalid request %#v = %#v, %v", request, token, err)
		}
	}
	if _, err := ledger.Reserve(valid); err != nil {
		t.Fatalf("Reserve valid request: %v", err)
	}
	if token, err := ledger.Reserve(valid); token != (Token{}) || !errors.Is(err, ErrDuplicateIdentity) {
		t.Fatalf("Reserve duplicate identity = %#v, %v", token, err)
	}

	full, fullCheckpoint := initializedLedger(t)
	for i := uint64(0); i < MaxRecords; i++ {
		if _, err := full.Reserve(allocationRequest(100 + i)); err != nil {
			t.Fatalf("Reserve slot %d: %v", i, err)
		}
	}
	_, savesBeforeFull := fullCheckpoint.snapshot()
	if token, err := full.Reserve(allocationRequest(100 + MaxRecords)); token != (Token{}) || !errors.Is(err, ErrCapacity) {
		t.Fatalf("Reserve at capacity = %#v, %v", token, err)
	}
	_, savesAfterFull := fullCheckpoint.snapshot()
	if savesAfterFull != savesBeforeFull {
		t.Fatalf("capacity refusal wrote checkpoint: saves %d -> %d", savesBeforeFull, savesAfterFull)
	}

	overflowState := checkpointState{Version: stateVersion, VaultID: allocationVaultID(), LastSequence: math.MaxUint64, Records: []Record{}}
	overflowCheckpoint := &fakeCheckpoint{initialized: true, loaded: true, payload: mustJSON(t, overflowState)}
	overflow, err := Open(overflowCheckpoint, allocationVaultID())
	if err != nil {
		t.Fatalf("Open max-sequence checkpoint: %v", err)
	}
	if token, err := overflow.Reserve(allocationRequest(500)); token != (Token{}) || !errors.Is(err, ErrSequenceExhausted) {
		t.Fatalf("Reserve after max sequence = %#v, %v", token, err)
	}
	_, overflowSaves := overflowCheckpoint.snapshot()
	if overflowSaves != 0 {
		t.Fatalf("sequence overflow wrote checkpoint: %d saves", overflowSaves)
	}
}

func TestRecordsAreClonedAndConcurrentReservationsSerialize(t *testing.T) {
	ledger, _ := initializedLedger(t)
	const count = 12
	var wg sync.WaitGroup
	errCh := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := ledger.Reserve(allocationRequest(uint64(600 + i)))
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Reserve: %v", err)
		}
	}
	records, err := ledger.Records()
	if err != nil || len(records) != count {
		t.Fatalf("Records = %d, %v", len(records), err)
	}
	for i, record := range records {
		if record.Token.Sequence != uint64(i+1) {
			t.Fatalf("record %d sequence = %d", i, record.Token.Sequence)
		}
	}
	records[0].Request.LogicalID = [16]byte{99}
	records[0].Locator = "mutated"
	again, err := ledger.Records()
	if err != nil || again[0].Request.LogicalID == ([16]byte{99}) || again[0].Locator == "mutated" {
		t.Fatalf("caller mutation leaked into ledger records: %#v, %v", again[0], err)
	}
}

func TestConfirmationRejectsInvalidLocatorAndCheckpointErrorsPoison(t *testing.T) {
	ledger, checkpoint := initializedLedger(t)
	request := allocationRequest(900)
	token, err := ledger.Reserve(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, locator := range []string{"", string(bytes.Repeat([]byte{'x'}, MaxLocatorLength+1)), "bad\x00locator", string([]byte{0xff})} {
		if record, err := ledger.Confirm(token, locator, request.ExpectedStoredLength, request.Digest); record != (Record{}) || !errors.Is(err, ErrInvalidLocator) {
			t.Errorf("Confirm locator %q = %#v, %v", locator, record, err)
		}
	}
	checkpoint.mu.Lock()
	checkpoint.checkErr = errors.New("lease lost")
	checkpoint.mu.Unlock()
	if err := ledger.Check(); !errors.Is(err, ErrCheckpointFailure) {
		t.Fatalf("Check lost lease = %v", err)
	}
	if _, err := ledger.Reserve(allocationRequest(901)); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("Reserve after lease loss = %v", err)
	}
}

func TestConfirmationCannotAliasAnotherAllocation(t *testing.T) {
	ledger, _ := initializedLedger(t)
	first, second := allocationRequest(910), allocationRequest(911)
	a, err := ledger.Reserve(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ledger.Reserve(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Confirm(a, "unique-physical-id", first.ExpectedStoredLength, first.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Confirm(b, "unique-physical-id", second.ExpectedStoredLength, second.Digest); !errors.Is(err, ErrConfirmationConflict) {
		t.Fatalf("aliased physical object accepted: %v", err)
	}
}

func TestIndependentLedgerCannotOverwriteNewerIntent(t *testing.T) {
	first, checkpoint := initializedLedger(t)
	second, err := Open(checkpoint, allocationVaultID())
	if err != nil {
		t.Fatal(err)
	}
	request := allocationRequest(920)
	if _, err := first.Reserve(request); err != nil {
		t.Fatal(err)
	}
	if records, err := second.Records(); records != nil || !errors.Is(err, ErrStaleLedger) {
		t.Fatalf("stale ledger exposed incomplete inventory: %+v %v", records, err)
	}
	if token, err := second.Reserve(allocationRequest(921)); token != (Token{}) || !errors.Is(err, ErrCheckpointFailure) {
		t.Fatalf("stale reservation overwrote checkpoint: %+v %v", token, err)
	}
	current, err := Open(checkpoint, allocationVaultID())
	if err != nil {
		t.Fatal(err)
	}
	records, err := current.Records()
	if err != nil || len(records) != 1 || records[0].Request != request {
		t.Fatalf("lost committed intent: %+v %v", records, err)
	}
}
