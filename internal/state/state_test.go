package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTripAndKeysAreIndependent(t *testing.T) {
	d := &Dir{Path: t.TempDir()}
	s := d.For("ports")
	if err := s.Save("current", []string{"tcp 0.0.0.0:22"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("other", 7); err != nil {
		t.Fatal(err)
	}
	var got []string
	if !s.Load("current", &got) || len(got) != 1 || got[0] != "tcp 0.0.0.0:22" {
		t.Fatalf("got %v", got)
	}
	var n int
	if !s.Load("other", &n) || n != 7 {
		t.Fatalf("second key lost: %d", n)
	}
}

func TestMissingKeyAndMissingFileReadAsNothingSaved(t *testing.T) {
	d := &Dir{Path: t.TempDir()}
	var v []string
	if d.For("nope").Load("x", &v) {
		t.Fatal("missing file must read as not found")
	}
}

func TestCorruptFileReadsAsFirstRunNotACrash(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "suid.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Dir{Path: dir}
	var v []string
	if d.For("suid").Load("files", &v) {
		t.Fatal("corrupt state must read as not found")
	}
	// And the next save repairs it.
	if err := d.For("suid").Save("files", []string{"/usr/bin/su"}); err != nil {
		t.Fatal(err)
	}
	if !d.For("suid").Load("files", &v) {
		t.Fatal("save after corruption did not repair the file")
	}
}

func TestOtherVersionIsIgnored(t *testing.T) {
	dir := t.TempDir()
	body := `{"version": 99, "data": {"files": ["/x"]}}`
	if err := os.WriteFile(filepath.Join(dir, "suid.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var v []string
	if (&Dir{Path: dir}).For("suid").Load("files", &v) {
		t.Fatal("a file from another envelope version must be ignored")
	}
}

func TestReadOnlyNeverWrites(t *testing.T) {
	dir := t.TempDir()
	d := &Dir{Path: dir, ReadOnly: true}
	if err := d.For("ports").Save("current", []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ports.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only store wrote a file")
	}
}

func TestFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	if err := (&Dir{Path: dir}).For("alerts").Save("conditions", 1); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "alerts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestSecondLockIsRefused(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v, want ErrLocked", err)
	}
	unlock()
	again, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	again()
}
