//go:build linux

package allocation

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/rclone/rclone/backend/degoo/internal/authority"
)

const (
	allocationChildEnv = "RCLONE_ALLOCATION_REOPEN_CHILD"
	testAuthorityVault = "allocation-subprocess-test-vault"
)

func TestAuthorityCheckpointSubprocessReopen(t *testing.T) {
	if path := os.Getenv(allocationChildEnv); path != "" {
		reopenAllocationInChild(t, path)
		return
	}
	requireAllocationTestHost(t)

	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "authority")
	if err := authority.Initialize(path, testAuthorityVault); err != nil {
		t.Fatalf("initialize authority: %v", err)
	}
	lease, err := authority.Open(context.Background(), path, testAuthorityVault)
	if err != nil {
		t.Fatalf("open authority: %v", err)
	}
	ledger, err := Initialize(lease, allocationVaultID())
	if err != nil {
		_ = lease.Close()
		t.Fatalf("initialize allocation ledger: %v", err)
	}
	request := allocationRequest(101)
	stale, err := Open(lease, allocationVaultID())
	if err != nil {
		_ = lease.Close()
		t.Fatal(err)
	}
	token, err := ledger.Reserve(request)
	if err != nil || token.Sequence != 1 {
		_ = lease.Close()
		t.Fatalf("reserve before restart = %#v, %v", token, err)
	}
	if token, err := stale.Reserve(allocationRequest(102)); err == nil || token != (Token{}) {
		_ = lease.Close()
		t.Fatalf("stale ledger overwrote real checkpoint: %+v %v", token, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("close authority before restart: %v", err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestAuthorityCheckpointSubprocessReopen$")
	command.Env = append(os.Environ(), allocationChildEnv+"="+path)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess reopen failed: %v\n%s", err, output)
	}
}

func reopenAllocationInChild(t *testing.T, path string) {
	t.Helper()
	lease, err := authority.Open(context.Background(), path, testAuthorityVault)
	if err != nil {
		t.Fatalf("open authority in child: %v", err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close child authority: %v", err)
		}
	}()
	ledger, err := Open(lease, allocationVaultID())
	if err != nil {
		t.Fatalf("open ledger in child: %v", err)
	}
	records, err := ledger.Records()
	want := allocationRequest(101)
	if err != nil || len(records) != 1 || records[0].Token.Sequence != 1 || records[0].Request != want || records[0].Confirmed {
		t.Fatalf("reopened allocation records = %#v, %v", records, err)
	}
}

func requireAllocationTestHost(t *testing.T) {
	t.Helper()
	const machineIDPath = "/etc/machine-id"
	info, err := os.Lstat(machineIDPath)
	if err != nil {
		t.Skipf("real authority checkpoint test requires a valid root-owned /etc/machine-id: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.Size() < 32 || info.Size() > 64 {
		t.Skipf("real authority checkpoint test requires a root-owned, non-writable regular /etc/machine-id")
	}
	data, err := os.ReadFile(machineIDPath)
	if err != nil {
		t.Skipf("read /etc/machine-id for real authority checkpoint test: %v", err)
	}
	id := strings.TrimSpace(string(data))
	if len(id) != 32 {
		t.Skipf("real authority checkpoint test requires 32 hexadecimal machine-id characters")
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Skipf("real authority checkpoint test requires hexadecimal machine-id characters: %v", err)
	}
}
