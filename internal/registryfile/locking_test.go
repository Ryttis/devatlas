//go:build linux

package registryfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Ryttis/devatlas/internal/registry"
)

func TestConfigAliases(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	a := NewStore(filepath.Join(real, "nested", "config.yaml"))
	b := NewStore(filepath.Join(alias, "nested", "config.yaml"))
	if a.Path() != b.Path() {
		t.Fatalf("different paths: %s %s", a.Path(), b.Path())
	}
	add(t, a, "p", "P", root)
	for _, kind := range []string{"symlink", "hardlink", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(root, kind)
			var err error
			if kind == "hardlink" {
				err = os.Link(a.Path(), path)
			} else {
				target := a.Path()
				if kind == "dangling" {
					target += "-absent"
				}
				err = os.Symlink(target, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			s := NewStore(path)
			if _, err := s.Load(); err == nil {
				t.Fatal("accepted file alias")
			}
			if err := s.Update(func(*registry.Registry) error { t.Fatal("mutation reached"); return nil }); err == nil {
				t.Fatal("updated alias")
			}
		})
	}
}

func TestLockSurvivesAtomicReplacementAndTimesOut(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	unlock, err := lock(path+".lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	before, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != 0600 {
		t.Fatalf("lock permissions: %v", before.Mode())
	}
	for i := 0; i < 2; i++ {
		if err := writeAtomic(path, []byte("schema_version: 1\n")); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		next, err := lock(path+".lock", 60*time.Millisecond)
		if err == nil {
			next()
			t.Fatal("lock lost across rename")
		}
		if !strings.Contains(err.Error(), "held it") || time.Since(start) > time.Second {
			t.Fatalf("unbounded or wrong error: %v", err)
		}
	}
	after, err := os.Stat(path + ".lock")
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("lock inode changed")
	}
}

func TestAtomicRenameFailureCleansTemporaryFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(target, []byte("secret")); err == nil {
		t.Fatal("rename should fail")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "target" {
		t.Fatalf("temp files remain: %v", entries)
	}
}

func TestLockRejectsAliasesAndSpecialFiles(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"symlink", "hardlink", "fifo"} {
		path := filepath.Join(root, kind)
		var err error
		if kind == "symlink" {
			err = os.Symlink(target, path)
		} else if kind == "fifo" {
			err = syscall.Mkfifo(path, 0600)
		} else {
			err = os.Link(target, path)
		}
		if err != nil {
			t.Fatal(err)
		}
		unlock, err := lock(path, time.Second)
		if err == nil {
			unlock()
			t.Fatalf("accepted %s lock", kind)
		}
	}
}

func TestUnavailableCheckoutDoesNotBlockUnrelatedUpdate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires unprivileged permissions")
	}
	s, root := newTestStore(t)
	parent := filepath.Join(root, "private")
	dir := filepath.Join(parent, "repo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	add(t, s, "p", "P", dir)
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0700)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("fixture: %v", err)
	}
	add(t, s, "q", "Q", t.TempDir())
	r, err := s.Load()
	if err != nil || len(r.Checkouts) != 2 {
		t.Fatalf("registration lost: %+v %v", r, err)
	}
}
