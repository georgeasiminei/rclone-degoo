//go:build linux

package authority

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const testVaultIdentity = "test-vault-identity"

func TestInitializeOpenClose(t *testing.T) {
	path := newAuthorityPath(t)
	if _, err := Open(context.Background(), path, testVaultIdentity); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Open on missing authority error = %v, want ErrNotInitialized", err)
	}
	if err := Initialize(path, testVaultIdentity); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(path, testVaultIdentity); err == nil {
		t.Fatal("Initialize replaced an existing authority")
	}
	checkMode(t, path, dirMode)
	checkMode(t, filepath.Join(path, lockName), fileMode)
	checkMode(t, filepath.Join(path, bindingName), fileMode)

	lease, err := Open(context.Background(), path, testVaultIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	lockPath := filepath.Join(path, lockName)
	before := statIdentity(t, lockPath)
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Check(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Check after Close = %v, want ErrClosed", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := statIdentity(t, lockPath); got != before {
		t.Fatalf("Close changed persistent lock inode: before %+v, after %+v", before, got)
	}
	lease, err = Open(context.Background(), path, testVaultIdentity)
	if err != nil {
		t.Fatalf("reopen after release: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSubprocessLockAndIndependentCaches(t *testing.T) {
	requireSupportedHost(t)
	parent := t.TempDir()
	state := filepath.Join(parent, "state")
	if err := os.Mkdir(state, dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, dirMode); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "authority")
	cacheA, cacheB := filepath.Join(parent, "cache-a"), filepath.Join(parent, "cache-b")
	for _, dir := range []string{cacheA, cacheB} {
		if err := os.Mkdir(dir, dirMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := Initialize(path, testVaultIdentity); err != nil {
		t.Fatal(err)
	}
	clientA := struct{ cache, authority string }{cache: cacheA, authority: path}
	clientB := struct{ cache, authority string }{cache: cacheB, authority: path}
	if clientA.cache == clientB.cache || clientA.authority != clientB.authority {
		t.Fatal("fixture must use distinct caches and one shared authority path")
	}
	if info, err := os.Stat(clientA.cache); err != nil || !info.IsDir() {
		t.Fatalf("first client cache is unavailable: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.v", "-test.run=^TestAuthorityLockHelper$")
	cmd.Env = append(os.Environ(),
		"RCLONE_DEGOO_AUTHORITY_HELPER=1",
		"RCLONE_DEGOO_AUTHORITY_PATH="+clientB.authority,
		"RCLONE_DEGOO_AUTHORITY_VAULT="+testVaultIdentity,
		"RCLONE_DEGOO_AUTHORITY_CACHE="+clientB.cache,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "LOCKED" {
				ready <- nil
				return
			}
		}
		ready <- fmt.Errorf("helper exited before locking: %w", scanner.Err())
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("wait for helper: %v; stderr: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper did not acquire lock; stderr: %s", stderr.String())
	}

	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if lease, err := Open(ctx, clientA.authority, testVaultIdentity); !errors.Is(err, context.DeadlineExceeded) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatalf("second cache acquired a held lock: lease=%v err=%v", lease, err)
	}
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v; stderr: %s", err, stderr.String())
	}

	lease, err := Open(context.Background(), path, testVaultIdentity)
	if err != nil {
		t.Fatalf("acquire after helper release: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorityLockHelper(t *testing.T) {
	if os.Getenv("RCLONE_DEGOO_AUTHORITY_HELPER") != "1" {
		return
	}
	if info, err := os.Stat(os.Getenv("RCLONE_DEGOO_AUTHORITY_CACHE")); err != nil || !info.IsDir() {
		t.Fatalf("helper cache is unavailable: %v", err)
	}
	lease, err := Open(context.Background(), os.Getenv("RCLONE_DEGOO_AUTHORITY_PATH"), os.Getenv("RCLONE_DEGOO_AUTHORITY_VAULT"))
	if err != nil {
		t.Fatalf("helper Open: %v", err)
	}
	fmt.Println("LOCKED")
	var signal [1]byte
	if _, err := os.Stdin.Read(signal[:]); err != nil {
		t.Fatalf("helper wait: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("helper Close: %v", err)
	}
}

func TestOpenFailsClosed(t *testing.T) {
	requireSupportedHost(t)
	t.Run("missing lock is not recreated", func(t *testing.T) {
		path := initializedAuthority(t)
		if err := os.Remove(filepath.Join(path, lockName)); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), path, testVaultIdentity); !errors.Is(err, ErrNotInitialized) {
			t.Fatalf("Open error = %v, want ErrNotInitialized", err)
		}
		if _, err := os.Lstat(filepath.Join(path, lockName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Open recreated missing lock: lstat error = %v", err)
		}
	})
	t.Run("corrupt binding", func(t *testing.T) {
		path := initializedAuthority(t)
		if err := os.WriteFile(filepath.Join(path, bindingName), []byte("{"), fileMode); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), path, testVaultIdentity); err == nil {
			t.Fatal("Open accepted corrupt binding")
		}
	})
	t.Run("missing binding is not recreated", func(t *testing.T) {
		path := initializedAuthority(t)
		bindingPath := filepath.Join(path, bindingName)
		if err := os.Remove(bindingPath); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), path, testVaultIdentity); !errors.Is(err, ErrNotInitialized) {
			t.Fatalf("Open error = %v, want ErrNotInitialized", err)
		}
		if _, err := os.Lstat(bindingPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Open recreated missing binding: lstat error = %v", err)
		}
	})
	t.Run("binding symlink", func(t *testing.T) {
		path := initializedAuthority(t)
		bindingPath := filepath.Join(path, bindingName)
		if err := os.Rename(bindingPath, bindingPath+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(bindingPath+".saved", bindingPath); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), path, testVaultIdentity); err == nil {
			t.Fatal("Open followed binding symlink")
		}
	})
	t.Run("lock symlink", func(t *testing.T) {
		path := initializedAuthority(t)
		lockPath := filepath.Join(path, lockName)
		if err := os.Rename(lockPath, lockPath+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(lockPath+".saved", lockPath); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), path, testVaultIdentity); err == nil {
			t.Fatal("Open followed lock symlink")
		}
	})
	t.Run("symlink path component", func(t *testing.T) {
		parent := t.TempDir()
		actualParent := filepath.Join(parent, "actual")
		if err := os.Mkdir(actualParent, dirMode); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(actualParent, "authority")
		if err := Initialize(path, testVaultIdentity); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(parent, "link")
		if err := os.Symlink(actualParent, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), filepath.Join(link, "authority"), testVaultIdentity); err == nil {
			t.Fatal("Open followed authority path symlink")
		}
	})
}

func TestBindingRefusesDifferentIdentity(t *testing.T) {
	requireSupportedHost(t)
	t.Run("vault", func(t *testing.T) {
		path := initializedAuthority(t)
		if _, err := Open(context.Background(), path, "another-vault"); !errors.Is(err, ErrBindingMismatch) {
			t.Fatalf("Open error = %v, want ErrBindingMismatch", err)
		}
	})
	t.Run("machine id and UID", func(t *testing.T) {
		for _, field := range []string{"machine_id", "owner_uid"} {
			t.Run(field, func(t *testing.T) {
				path := initializedAuthority(t)
				bindingPath := filepath.Join(path, bindingName)
				data, err := os.ReadFile(bindingPath)
				if err != nil {
					t.Fatal(err)
				}
				var values map[string]any
				if err := json.Unmarshal(data, &values); err != nil {
					t.Fatal(err)
				}
				if field == "machine_id" {
					values[field] = strings.Repeat("0", 32)
				} else {
					values[field] = float64(uint32(os.Geteuid()) + 1)
				}
				data, err = json.Marshal(values)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(bindingPath, data, fileMode); err != nil {
					t.Fatal(err)
				}
				if _, err := Open(context.Background(), path, testVaultIdentity); err == nil {
					t.Fatalf("Open accepted changed %s binding", field)
				}
			})
		}
	})
	t.Run("canonical path", func(t *testing.T) {
		parent := t.TempDir()
		state := filepath.Join(parent, "state")
		if err := os.Mkdir(state, dirMode); err != nil {
			t.Fatal(err)
		}
		original := filepath.Join(state, "authority")
		moved := filepath.Join(state, "moved")
		if err := Initialize(original, testVaultIdentity); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(original, moved); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), moved, testVaultIdentity); !errors.Is(err, ErrBindingMismatch) {
			t.Fatalf("Open at moved path = %v, want ErrBindingMismatch", err)
		}
	})
}

func TestPrivateModesAndLockReplacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		mode os.FileMode
	}{
		{name: "authority directory", file: ".", mode: 0750},
		{name: "binding file", file: bindingName, mode: 0640},
		{name: "lock file", file: lockName, mode: 0640},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := initializedAuthority(t)
			if err := os.Chmod(filepath.Join(path, tc.file), tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(context.Background(), path, testVaultIdentity); err == nil {
				t.Fatal("Open accepted non-private permissions")
			}
		})
	}
	t.Run("replacement rejected by current and future leases", func(t *testing.T) {
		path := initializedAuthority(t)
		lease, err := Open(context.Background(), path, testVaultIdentity)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := lease.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		lockPath := filepath.Join(path, lockName)
		if err := os.Rename(lockPath, lockPath+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(lockPath, nil, fileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lockPath, fileMode); err != nil {
			t.Fatal(err)
		}
		if err := lease.Check(); !errors.Is(err, ErrReplaced) {
			t.Fatalf("Check after replacement = %v, want ErrReplaced", err)
		}
		if _, err := Open(context.Background(), path, testVaultIdentity); !errors.Is(err, ErrReplaced) {
			t.Fatalf("Open after replacement = %v, want ErrReplaced", err)
		}
	})
}

func initializedAuthority(t *testing.T) string {
	t.Helper()
	path := newAuthorityPath(t)
	if err := Initialize(path, testVaultIdentity); err != nil {
		t.Fatal(err)
	}
	return path
}

func newAuthorityPath(t *testing.T) string {
	t.Helper()
	requireSupportedHost(t)
	parent := t.TempDir()
	state := filepath.Join(parent, "state")
	if err := os.Mkdir(state, dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, dirMode); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(state, "authority")
}

func requireSupportedHost(t *testing.T) {
	t.Helper()
	if _, err := localMachineID(); err != nil {
		t.Skipf("Linux writer-authority tests require a valid root-owned /etc/machine-id: %v", err)
	}
}

func checkMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode for %s = %#o, want %#o", path, got, want)
	}
}

func statIdentity(t *testing.T, path string) fileIdentity {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFD(fd)
	st, err := statFD(fd)
	if err != nil {
		t.Fatal(err)
	}
	return identityOf(&st)
}
