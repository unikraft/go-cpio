// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, Unikraft GmbH and The Unikraft CLI Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package cpio_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unikraft/go-cpio"
)

// writeEntry writes a single cpio entry with the given full path (Name is used
// verbatim, preserving any directory prefix). For symlinks the target is
// written as the entry body, matching the SVR4 on-disk layout.
func writeEntry(w *cpio.Writer, name string, mode cpio.FileMode, mtime time.Time, body, link string) error {
	hdr := &cpio.Header{
		Name:    name,
		Mode:    mode,
		ModTime: mtime,
		Size:    int64(len(body)),
		Links:   1,
	}
	if link != "" {
		hdr.Mode |= cpio.TypeSymlink
		hdr.Linkname = link
	}
	if err := w.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.WriteString(w, body); err != nil {
		return err
	}
	return nil
}

func TestUnpack(t *testing.T) {
	mtime := time.Unix(1_700_000_000, 0).UTC()

	var buf bytes.Buffer
	w := cpio.NewWriter(&buf)
	entries := []struct {
		name string
		mode cpio.FileMode
		body string
		link string
	}{
		{"dir/", cpio.TypeDir | 0o755, "", ""},
		{"dir/hello.txt", cpio.TypeRegular | 0o644, "hello world\n", ""},
		{"dir/sub/", cpio.TypeDir | 0o755, "", ""},
		{"dir/sub/nested.txt", cpio.TypeRegular | 0o600, "nested\n", ""},
		{"dir/link.txt", 0o777, "target.txt", "target.txt"},
	}
	for _, e := range entries {
		if err := writeEntry(w, e.name, e.mode, mtime, e.body, e.link); err != nil {
			t.Fatalf("writing %q: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing writer: %v", err)
	}

	dest := t.TempDir()
	if err := cpio.Unpack(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// Regular file content round-trips intact.
	got, err := os.ReadFile(filepath.Join(dest, "dir/hello.txt"))
	if err != nil {
		t.Fatalf("reading extracted hello.txt: %v", err)
	}
	if want := "hello world\n"; string(got) != want {
		t.Errorf("hello.txt content = %q, want %q", got, want)
	}

	gotNested, err := os.ReadFile(filepath.Join(dest, "dir/sub/nested.txt"))
	if err != nil {
		t.Fatalf("reading extracted nested.txt: %v", err)
	}
	if want := "nested\n"; string(gotNested) != want {
		t.Errorf("nested.txt content = %q, want %q", gotNested, want)
	}

	// Nested directory was created.
	if fi, err := os.Stat(filepath.Join(dest, "dir/sub")); err != nil || !fi.IsDir() {
		t.Errorf("dir/sub not extracted as directory (fi=%v, err=%v)", fi, err)
	}

	// Symlink target is preserved.
	gotLink, err := os.Readlink(filepath.Join(dest, "dir/link.txt"))
	if err != nil {
		t.Fatalf("reading symlink: %v", err)
	}
	if gotLink != "target.txt" {
		t.Errorf("symlink target = %q, want %q", gotLink, "target.txt")
	}

	// File mode (perm bits) is preserved.
	fi, err := os.Stat(filepath.Join(dest, "dir/sub/nested.txt"))
	if err != nil {
		t.Fatalf("stat nested.txt: %v", err)
	}
	if got, want := fi.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("nested.txt perm = %v, want %v", got, want)
	}

	// mtime is preserved on regular files.
	if got, want := fi.ModTime(), mtime; !got.Equal(want) {
		t.Errorf("nested.txt mtime = %v, want %v", got, want)
	}
}

func TestUnpack_pathEscape(t *testing.T) {
	var buf bytes.Buffer
	w := cpio.NewWriter(&buf)
	// The "../" components are neutralized by safeJoin, so the entry must land
	// directly under dest (as "evil"), never above it.
	if err := writeEntry(w, "../../evil", cpio.TypeRegular|0o644, time.Now(), "pwned", ""); err != nil {
		t.Fatalf("writeEntry: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing writer: %v", err)
	}

	dest := t.TempDir()
	if err := cpio.Unpack(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// The file must exist INSIDE dest...
	got, err := os.ReadFile(filepath.Join(dest, "evil"))
	if err != nil {
		t.Fatalf("evil not extracted inside dest: %v", err)
	}
	if string(got) != "pwned" {
		t.Errorf("evil content = %q, want %q", got, "pwned")
	}

	// ...and NOT above dest.
	parent := filepath.Dir(dest)
	if _, statErr := os.Stat(filepath.Join(parent, "evil")); statErr == nil {
		t.Error("escaping file was written above dest")
	}
}

func TestUnpack_empty(t *testing.T) {
	dest := t.TempDir()
	// An empty reader yields no entries and no error.
	if err := cpio.Unpack(bytes.NewReader(nil), dest); err != nil {
		t.Fatalf("Unpack of empty stream: %v", err)
	}
}

// TestUnpack_setuidSticky verifies that special mode bits (setuid/setgid/
// sticky) survive extraction, not just the permission bits.
func TestUnpack_setuidSticky(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: setuid bit enforcement differs")
	}
	mtime := time.Unix(1_700_000_000, 0).UTC()

	var buf bytes.Buffer
	w := cpio.NewWriter(&buf)
	entries := []struct {
		name string
		mode cpio.FileMode
		body string
	}{
		{"suid", cpio.TypeRegular | cpio.ModeSetuid | 0o755, "s"},
		{"sgid", cpio.TypeRegular | cpio.ModeSetgid | 0o755, "g"},
		{"sticky_dir/", cpio.TypeDir | cpio.ModeSticky | 0o777, ""},
	}
	for _, e := range entries {
		if err := writeEntry(w, e.name, e.mode, mtime, e.body, ""); err != nil {
			t.Fatalf("writing %q: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing writer: %v", err)
	}

	dest := t.TempDir()
	if err := cpio.Unpack(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dest, "suid"))
	if err != nil {
		t.Fatalf("stat suid: %v", err)
	}
	if fi.Mode()&os.ModeSetuid == 0 {
		t.Errorf("suid: setuid bit not preserved (mode=%v)", fi.Mode())
	}

	fi, err = os.Stat(filepath.Join(dest, "sgid"))
	if err != nil {
		t.Fatalf("stat sgid: %v", err)
	}
	if fi.Mode()&os.ModeSetgid == 0 {
		t.Errorf("sgid: setgid bit not preserved (mode=%v)", fi.Mode())
	}

	fi, err = os.Stat(filepath.Join(dest, "sticky_dir"))
	if err != nil {
		t.Fatalf("stat sticky_dir: %v", err)
	}
	if fi.Mode()&os.ModeSticky == 0 {
		t.Errorf("sticky_dir: sticky bit not preserved (mode=%v)", fi.Mode())
	}
}

// TestUnpack_symlinkTraversalEscape verifies that an archive cannot plant a
// symlinked directory and then write a file through it to escape dest on disk.
func TestUnpack_symlinkTraversalEscape(t *testing.T) {
	target := t.TempDir() // outside dest; must stay untouched
	targetFile := filepath.Join(target, "smuggled")
	mtime := time.Unix(1_700_000_000, 0).UTC()

	var buf bytes.Buffer
	w := cpio.NewWriter(&buf)
	// "out" is a symlink pointing at the external target dir...
	if err := writeEntry(w, "out", 0o777, mtime, target, target); err != nil {
		t.Fatalf("writeEntry out: %v", err)
	}
	// ...then a file written through "out" would land in target.
	if err := writeEntry(w, "out/smuggled", cpio.TypeRegular|0o644, mtime, "escaped", ""); err != nil {
		t.Fatalf("writeEntry smuggled: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing writer: %v", err)
	}

	dest := t.TempDir()
	err := cpio.Unpack(bytes.NewReader(buf.Bytes()), dest)
	if err == nil {
		t.Fatal("Unpack succeeded for symlink-traversal escape, want error")
	}

	if _, statErr := os.Stat(targetFile); statErr == nil {
		t.Errorf("file was written outside dest via symlink traversal")
	}
}
