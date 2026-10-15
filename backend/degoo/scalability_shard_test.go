package degoo

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"testing"
)

// This executable P1 model measures an encrypted copy-on-write radix index.
// Its in-memory ID store and root swap are assumptions, not Degoo operations.
const shardModelLeafEntries = 64
const shardModelMaxBytes = 128 * 1024

type shardModelRecord struct {
	Key  string
	Name string
	ID   string
}

type shardModelNode struct {
	Records  []shardModelRecord
	Children map[string]string
}

type shardModelCounters struct {
	Reads      int
	Writes     int
	ReadBytes  int
	WriteBytes int
}

type shardModel struct {
	aead       cipher.AEAD
	key        []byte
	objects    map[string][]byte
	root       string
	nextID     int
	failWrite  int
	maxRecords int
	maxBytes   int
	counts     shardModelCounters
}

func newShardModel(t *testing.T) *shardModel {
	t.Helper()
	key := bytes.Repeat([]byte{0x29}, 32) // Generated fixture key only.
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return &shardModel{aead: aead, key: key, objects: make(map[string][]byte)}
}

func (m *shardModel) logicalKey(name string) string {
	mac := hmac.New(sha256.New, m.key)
	_, _ = mac.Write([]byte("p1-index-key\x00" + name))
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *shardModel) store(node shardModelNode) (string, error) {
	if len(node.Records) > shardModelLeafEntries || len(node.Children) > 16 || (len(node.Records) > 0 && len(node.Children) > 0) {
		return "", errors.New("model node exceeds record bound")
	}
	plain, err := json.Marshal(node)
	if err != nil {
		return "", err
	}
	if len(plain)+m.aead.NonceSize()+m.aead.Overhead() > shardModelMaxBytes {
		return "", errors.New("model node exceeds encrypted byte bound")
	}
	if m.failWrite > 0 {
		m.failWrite--
		if m.failWrite == 0 {
			return "", errors.New("injected model write failure")
		}
	}
	m.nextID++
	id := fmt.Sprintf("model-id-%08d", m.nextID)
	nonce := make([]byte, m.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := m.aead.Seal(nonce, nonce, plain, []byte("p1-index-node\x00"+id))
	m.objects[id] = sealed
	m.counts.Writes++
	m.counts.WriteBytes += len(sealed)
	m.maxRecords = max(m.maxRecords, len(node.Records), len(node.Children))
	m.maxBytes = max(m.maxBytes, len(sealed))
	return id, nil
}

func (m *shardModel) load(id string) (shardModelNode, error) {
	sealed, ok := m.objects[id]
	if !ok || len(sealed) < m.aead.NonceSize()+m.aead.Overhead() || len(sealed) > shardModelMaxBytes {
		return shardModelNode{}, errors.New("missing or invalid model node")
	}
	m.counts.Reads++
	m.counts.ReadBytes += len(sealed)
	nonce := sealed[:m.aead.NonceSize()]
	plain, err := m.aead.Open(nil, nonce, sealed[m.aead.NonceSize():], []byte("p1-index-node\x00"+id))
	if err != nil {
		return shardModelNode{}, err
	}
	var node shardModelNode
	err = json.Unmarshal(plain, &node)
	return node, err
}

func (m *shardModel) build(records []shardModelRecord, depth int) (string, error) {
	if len(records) <= shardModelLeafEntries {
		return m.store(shardModelNode{Records: records})
	}
	if depth >= sha256.Size*2 {
		return "", errors.New("model hash collision exceeds leaf bound")
	}
	groups := make(map[string][]shardModelRecord)
	for _, record := range records {
		digit := record.Key[depth : depth+1]
		groups[digit] = append(groups[digit], record)
	}
	children := make(map[string]string)
	for _, digit := range sortedShardModelKeys(groups) {
		id, err := m.build(groups[digit], depth+1)
		if err != nil {
			return "", err
		}
		children[digit] = id
	}
	return m.store(shardModelNode{Children: children})
}

func sortedShardModelKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (m *shardModel) lookup(root, name string) (shardModelRecord, error) {
	key := m.logicalKey(name)
	for depth := 0; root != "" && depth <= len(key); depth++ {
		node, err := m.load(root)
		if err != nil {
			return shardModelRecord{}, err
		}
		if len(node.Children) == 0 {
			for _, record := range node.Records {
				if record.Key == key && record.Name == name {
					return record, nil
				}
			}
			break
		}
		if depth == len(key) {
			return shardModelRecord{}, errors.New("model trie depth exceeded")
		}
		root = node.Children[key[depth:depth+1]]
	}
	return shardModelRecord{}, os.ErrNotExist
}

// replace writes descendants before their parent and returns an unpublished root.
func (m *shardModel) replace(id string, record shardModelRecord, remove bool, depth int) (string, error) {
	var node shardModelNode
	var err error
	if id != "" {
		node, err = m.load(id)
		if err != nil {
			return "", err
		}
	}
	if len(node.Children) == 0 {
		records := make([]shardModelRecord, 0, len(node.Records)+1)
		for _, existing := range node.Records {
			if existing.Key != record.Key {
				records = append(records, existing)
			}
		}
		if !remove {
			records = append(records, record)
		}
		return m.build(records, depth)
	}
	if depth >= len(record.Key) {
		return "", errors.New("model trie depth exceeded")
	}
	digit := record.Key[depth : depth+1]
	child, err := m.replace(node.Children[digit], record, remove, depth+1)
	if err != nil {
		return "", err
	}
	node.Children[digit] = child
	return m.store(node)
}

func (m *shardModel) rebuild(id string, records map[string]string) error {
	node, err := m.load(id)
	if err != nil {
		return err
	}
	for _, record := range node.Records {
		if record.Key != m.logicalKey(record.Name) {
			return errors.New("model record key mismatch")
		}
		if _, exists := records[record.Name]; exists {
			return errors.New("duplicate model record")
		}
		records[record.Name] = record.ID
	}
	for _, digit := range sortedShardModelKeys(node.Children) {
		if err := m.rebuild(node.Children[digit], records); err != nil {
			return err
		}
	}
	return nil
}

func TestScalabilityEncryptedShardModel(t *testing.T) {
	for _, count := range []int{1000, 10000, 100000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			if count == 100000 && os.Getenv("RCLONE_DEGOO_SCALE_TESTS") != "1" {
				t.Skip("set RCLONE_DEGOO_SCALE_TESTS=1 for 100,000 model records")
			}
			m := newShardModel(t)
			records := make([]shardModelRecord, count)
			for index := range records {
				name := fmt.Sprintf("generated-logical-%08d", index)
				records[index] = shardModelRecord{Key: m.logicalKey(name), Name: name, ID: fmt.Sprintf("generated-data-%08d", index)}
			}
			root, err := m.build(records, 0)
			if err != nil {
				t.Fatal(err)
			}
			m.root = root
			build := m.counts
			buildObjects := len(m.objects)
			m.counts = shardModelCounters{}
			target := records[count-1]
			got, err := m.lookup(m.root, target.Name)
			if err != nil || got != target {
				t.Fatalf("cold model lookup mismatch: %v", err)
			}
			lookup := m.counts
			if lookup.Reads > sha256.Size*2+1 || lookup.Writes != 0 {
				t.Fatalf("unbounded lookup: %+v", lookup)
			}
			m.counts = shardModelCounters{}
			if _, err := m.lookup(m.root, "generated-missing"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing key: %v", err)
			}
			missing := m.counts
			m.counts = shardModelCounters{}
			target.ID = "generated-replacement"
			next, err := m.replace(m.root, target, false, 0)
			if err != nil {
				t.Fatal(err)
			}
			update := m.counts
			if update.Writes != lookup.Reads || update.Reads != lookup.Reads {
				t.Fatalf("overwrite fan-out %+v vs lookup %+v", update, lookup)
			}
			old, err := m.lookup(m.root, target.Name)
			if err != nil || old.ID != records[count-1].ID {
				t.Fatalf("staged update changed old root: %v", err)
			}
			m.root = next // Assumed atomic root selection; no provider claim.
			got, err = m.lookup(m.root, target.Name)
			if err != nil || got != target {
				t.Fatalf("published update mismatch: %v", err)
			}
			m.counts = shardModelCounters{}
			renamed := target
			renamed.Name = "generated-renamed"
			renamed.Key = m.logicalKey(renamed.Name)
			withoutSource, err := m.replace(m.root, target, true, 0)
			if err != nil {
				t.Fatal(err)
			}
			renamedRoot, err := m.replace(withoutSource, renamed, false, 0)
			if err != nil {
				t.Fatal(err)
			}
			rename := m.counts
			if _, err := m.lookup(renamedRoot, target.Name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rename retained source: %v", err)
			}
			if got, err := m.lookup(renamedRoot, renamed.Name); err != nil || got.ID != target.ID {
				t.Fatalf("rename lost stable data ID: %v", err)
			}
			// Rebuild the overwrite root, independently of staged rename pages.
			m.counts = shardModelCounters{}
			recovered := make(map[string]string)
			if err := m.rebuild(m.root, recovered); err != nil || len(recovered) != count {
				t.Fatalf("cold model rebuild: count=%d error=%v", len(recovered), err)
			}
			for _, record := range records[:count-1] {
				if recovered[record.Name] != record.ID {
					t.Fatal("rebuild lost or mixed a record")
				}
			}
			if recovered[target.Name] != target.ID {
				t.Fatal("rebuild lost updated record")
			}
			rebuild := m.counts
			if rebuild.Reads != buildObjects || m.maxRecords > shardModelLeafEntries || m.maxBytes > shardModelMaxBytes {
				t.Fatalf("unexpected rebuild or bounds: %+v", rebuild)
			}
			for _, sealed := range m.objects {
				if bytes.Contains(sealed, []byte("generated-logical-")) || bytes.Contains(sealed, []byte("generated-data-")) {
					t.Fatal("plaintext metadata in encrypted model store")
				}
			}
			evidence := map[string]any{"entries": count, "build": build, "build_objects": buildObjects, "cold_lookup": lookup, "missing_lookup": missing, "overwrite": update, "rename": rename, "rebuild": rebuild, "max_node_records": m.maxRecords, "max_encrypted_node_bytes": m.maxBytes, "provider_requests_measured": false}
			encoded, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			t.Log(string(encoded))
		})
	}
}

func TestScalabilityShardModelSplit(t *testing.T) {
	m := newShardModel(t)
	records := make([]shardModelRecord, shardModelLeafEntries)
	for index := range records {
		name := fmt.Sprintf("generated-split-%03d", index)
		records[index] = shardModelRecord{Key: m.logicalKey(name), Name: name, ID: "generated-data"}
	}
	root, err := m.build(records, 0)
	if err != nil {
		t.Fatal(err)
	}
	extra := shardModelRecord{Key: m.logicalKey("generated-extra"), Name: "generated-extra", ID: "generated-extra-data"}
	m.counts = shardModelCounters{}
	next, err := m.replace(root, extra, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	split := m.counts
	if split.Reads != 1 || split.Writes > 17 || split.Writes < 2 {
		t.Fatalf("65-entry split fan-out: %+v", split)
	}
	if _, err := m.lookup(root, extra.Name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("split mutated previous root: %v", err)
	}
	recovered := make(map[string]string)
	if err := m.rebuild(next, recovered); err != nil || len(recovered) != 65 {
		t.Fatalf("split lost records: %d, %v", len(recovered), err)
	}
	for boundary := 1; boundary <= split.Writes; boundary++ {
		before := len(m.objects)
		m.failWrite = boundary
		if _, err := m.replace(root, extra, false, 0); err == nil {
			t.Fatal("injected split write unexpectedly succeeded")
		}
		if len(m.objects)-before != boundary-1 {
			t.Fatal("unexpected interrupted split object count")
		}
		original := make(map[string]string)
		if err := m.rebuild(root, original); err != nil || len(original) != shardModelLeafEntries {
			t.Fatalf("interrupted split changed previous root: %v", err)
		}
	}
	// Fully colliding keys must fail at a finite depth, retaining the old root.
	colliding := append(append([]shardModelRecord(nil), records...), extra)
	for index := range colliding {
		colliding[index].Key = records[0].Key
	}
	if _, err := m.build(colliding, 0); err == nil {
		t.Fatal("hash collision exceeded leaf bound")
	}
	oversize := shardModelRecord{Key: extra.Key, Name: string(bytes.Repeat([]byte{'x'}, shardModelMaxBytes)), ID: extra.ID}
	if _, err := m.store(shardModelNode{Records: []shardModelRecord{oversize}}); err == nil {
		t.Fatal("oversized encrypted record accepted")
	}
	t.Logf("65-entry leaf split: reads=%d writes=%d encrypted_bytes=%d; previous root remains readable", split.Reads, split.Writes, split.WriteBytes)
}

func TestScalabilityShardModelFailure(t *testing.T) {
	m := newShardModel(t)
	records := make([]shardModelRecord, 65)
	for index := range records {
		name := fmt.Sprintf("generated-%03d", index)
		records[index] = shardModelRecord{Key: m.logicalKey(name), Name: name, ID: "generated-original"}
	}
	root, err := m.build(records, 0)
	if err != nil {
		t.Fatal(err)
	}
	m.root = root
	for boundary := 1; boundary <= 2; boundary++ {
		target := records[0]
		target.ID = "generated-replacement"
		before := len(m.objects)
		m.failWrite = boundary
		if _, err := m.replace(m.root, target, false, 0); err == nil {
			t.Fatal("injected write unexpectedly succeeded")
		}
		got, err := m.lookup(m.root, target.Name)
		if err != nil || got != records[0] || len(m.objects)-before != boundary-1 {
			t.Fatalf("failed publication visibility or staged-object count: %v", err)
		}
	}
	sealed := append([]byte(nil), m.objects[root]...)
	m.objects[root][len(sealed)-1] ^= 1
	if _, err := m.lookup(root, records[0].Name); err == nil {
		t.Fatal("authenticated corruption accepted")
	}
	m.objects[root] = sealed
	child := ""
	node, err := m.load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range node.Children {
		if child == "" {
			child = id
		}
	}
	m.objects[child] = sealed
	if _, err := m.load(child); err == nil {
		t.Fatal("cross-ID ciphertext substitution accepted")
	}
	delete(m.objects, child)
	if err := m.rebuild(root, make(map[string]string)); err == nil {
		t.Fatal("missing node silently accepted during rebuild")
	}
}
