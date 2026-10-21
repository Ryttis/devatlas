//go:build linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Ryttis/devatlas/internal/httpserver"
)

// These are independent OS processes running the real CLI executable, not
// goroutines sharing a Store or the testing process's file descriptors.
func TestConcurrentCLIProcesses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := t.TempDir()
	binary := filepath.Join(root, "devatlas")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/devatlas")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	config := filepath.Join(root, "registrations.yaml")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(config+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	const n = 16
	type process struct {
		cmd    *exec.Cmd
		output bytes.Buffer
		done   chan error
	}
	children := make([]*process, n)
	var wg sync.WaitGroup
	for i := range children {
		dir := filepath.Join(root, fmt.Sprintf("repo-%02d", i))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		cfg := config
		if i%2 == 1 {
			cfg = filepath.Join(alias, "registrations.yaml")
		}
		child := &process{cmd: exec.CommandContext(ctx, binary, "add", "-config", cfg, "-project", "p", "-name", "P", dir), done: make(chan error, 1)}
		child.cmd.Stdout = &child.output
		child.cmd.Stderr = &child.output
		if err := child.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.cmd.Process.Kill() })
		children[i] = child
		wg.Add(1)
		go func() { defer wg.Done(); child.done <- child.cmd.Wait() }()
	}
	// All writers must wait for the lock before reading. Replace the registry
	// while holding that lock; every writer must retain this newly added record.
	time.Sleep(150 * time.Millisecond)
	for _, child := range children {
		select {
		case err := <-child.done:
			t.Fatalf("CLI bypassed held lock: %v %s", err, child.output.String())
		default:
		}
	}
	if err := os.WriteFile(config, []byte("schema_version: 1\nprojects: [{id: seed, name: Seed}]\ncheckouts: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for _, child := range children {
		if err := <-child.done; err != nil {
			t.Fatalf("CLI: %v %s", err, child.output.String())
		}
	}
	output, err := exec.CommandContext(ctx, binary, "list", "-config", config, "-json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var response httpserver.ProjectsResponse
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Projects) != 2 || response.Projects[0].ID != "p" || len(response.Projects[0].Checkouts) != n || response.Projects[1].ID != "seed" {
		t.Fatalf("lost updates: %s", output)
	}

	// Concurrent add/remove must also preserve both sets of mutations.
	children = nil
	for i, c := range response.Projects[0].Checkouts {
		args := []string{"remove", "-config", config, c.ID}
		if i%2 == 1 {
			dir := filepath.Join(root, fmt.Sprintf("extra-%02d", i))
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			args = []string{"add", "-config", config, "-project", "p", dir}
		}
		child := &process{cmd: exec.CommandContext(ctx, binary, args...)}
		child.cmd.Stdout = &child.output
		child.cmd.Stderr = &child.output
		if err := child.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.cmd.Process.Kill() })
		children = append(children, child)
	}
	for _, child := range children {
		if err := child.cmd.Wait(); err != nil {
			t.Fatalf("CLI: %v %s", err, child.output.String())
		}
	}
	output, err = exec.CommandContext(ctx, binary, "list", "-config", config, "-json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var after httpserver.ProjectsResponse
	if err := json.Unmarshal(output, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Projects) != 2 || len(after.Projects[0].Checkouts) != n {
		t.Fatalf("lost mixed updates: %s", output)
	}
	ids := map[string]bool{}
	for _, c := range after.Projects[0].Checkouts {
		ids[c.ID] = true
	}
	for i, c := range response.Projects[0].Checkouts {
		if ids[c.ID] != (i%2 == 1) {
			t.Fatalf("wrong removal result for %s", c.ID)
		}
	}
}
