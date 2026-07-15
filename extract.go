// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, Unikraft GmbH and The Unikraft CLI Authors.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package cpio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type dirMeta struct {
	path  string
	mode  FileMode
	mtime time.Time
	uid   int
	gid   int
}

// Unpack reads a CPIO archive from r and extracts its entries under dest.
// Directories and regular files are restored with their permission bits
// and modification time. Ownership is restored best-effort.
// Device, fifo, and socket entries are skipped.
func Unpack(r io.Reader, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("creating destination directory: %w", err)
	}

	// Directory metadata is applied in a final pass: creating a directory's
	// children updates its mtime, so setting it eagerly would be overwritten.
	var dirs []dirMeta

	cr := NewReader(r)
	for {
		hdr, err := cr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.Join(err, applyDirMeta(dirs))
		}

		p, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return errors.Join(err, applyDirMeta(dirs))
		}
		if p == "" {
			continue
		}

		if err := ensureNoSymlinkParents(dest, p); err != nil {
			return errors.Join(err, applyDirMeta(dirs))
		}

		perm := os.FileMode(hdr.Mode).Perm()

		// Mask the type bits before comparing so the type check is exact: a
		// regular file (0o100000) must not be mistaken for a symlink (0o120000)
		// just because they share high bits.
		switch hdr.Mode & ModeType {
		case TypeDir:
			if err := os.MkdirAll(p, perm); err != nil {
				return errors.Join(fmt.Errorf("creating directory %q: %w", p, err), applyDirMeta(dirs))
			}
			dirs = append(dirs, dirMeta{path: p, mode: hdr.Mode, mtime: hdr.ModTime, uid: hdr.Uid, gid: hdr.Guid})

		case TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return errors.Join(fmt.Errorf("creating parent of %q: %w", p, err), applyDirMeta(dirs))
			}
			_ = os.Remove(p)
			if err := os.Symlink(hdr.Linkname, p); err != nil {
				return errors.Join(fmt.Errorf("creating symlink %q: %w", p, err), applyDirMeta(dirs))
			}
			tryChown(p, hdr.Uid, hdr.Guid)

		case TypeRegular:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return errors.Join(fmt.Errorf("creating parent of %q: %w", p, err), applyDirMeta(dirs))
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
			if err != nil {
				return errors.Join(fmt.Errorf("creating file %q: %w", p, err), applyDirMeta(dirs))
			}
			if _, err := io.Copy(f, cr); err != nil {
				f.Close()
				return errors.Join(fmt.Errorf("writing file %q: %w", p, err), applyDirMeta(dirs))
			}
			if err := f.Close(); err != nil {
				return errors.Join(fmt.Errorf("closing file %q: %w", p, err), applyDirMeta(dirs))
			}

			_ = os.Chmod(p, perm|specialBits(hdr.Mode))
			setMtime(p, hdr.ModTime)
			tryChown(p, hdr.Uid, hdr.Guid)

		default:
			// Skip device, char device, fifo, and socket entries.
		}
	}

	return applyDirMeta(dirs)
}

// applyDirMeta restores permission bits (incl. special bits), mtime, and
// ownership for directories recorded during the walk.
func applyDirMeta(dirs []dirMeta) error {
	var err error
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		perm := os.FileMode(d.mode).Perm() | specialBits(d.mode)
		err = errors.Join(err, os.Chmod(d.path, perm))
		setMtime(d.path, d.mtime)
		tryChown(d.path, d.uid, d.gid)
	}
	return err
}

// specialBits returns the setuid/setgid/sticky bits of mode as an os.FileMode.
func specialBits(mode FileMode) os.FileMode {
	var m os.FileMode
	if mode&ModeSetuid != 0 {
		m |= os.ModeSetuid
	}
	if mode&ModeSetgid != 0 {
		m |= os.ModeSetgid
	}
	if mode&ModeSticky != 0 {
		m |= os.ModeSticky
	}
	return m
}

// setMtime sets the access and modification times of p to mtime.
// Restoration is best-effort.
func setMtime(p string, mtime time.Time) {
	if mtime.IsZero() {
		return
	}
	_ = os.Chtimes(p, mtime, mtime)
}

// tryChown restores ownership best-effort.
func tryChown(p string, uid, gid int) {
	_ = os.Lchown(p, uid, gid)
}

// ensureNoSymlinkParents verifies that no component of p between dest and the
// final element is a symlink.
func ensureNoSymlinkParents(dest, p string) error {
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("resolving destination: %w", err)
	}
	absP, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", p, err)
	}

	rel, err := filepath.Rel(absDest, absP)
	if err != nil {
		return err
	}

	// Walk every intermediate component (everything except the final element)
	// and refuse if any is a symlink: the resolved path could leave dest.
	cur := absDest
	for _, seg := range splitSegments(rel) {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("entry %q traverses symlink %q", p, cur)
		}
	}
	return nil
}

// splitSegments returns the non-empty, non-"." path segments of rel, split on
// '/' (normalized from the OS separator first).
func splitSegments(rel string) []string {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "" || rel == "." {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			if i > start {
				if seg := rel[start:i]; seg != "." {
					out = append(out, seg)
				}
			}
			start = i + 1
		}
	}
	if start < len(rel) {
		if seg := rel[start:]; seg != "." {
			out = append(out, seg)
		}
	}
	return out
}

// safeJoin resolves an archive entry name to a path under dest, refusing any
// entry that would lexically escape dest.
func safeJoin(dest, name string) (string, error) {
	rel := filepath.Clean("/" + name)
	rel = rel[1:] // drop the forced leading "/"
	if rel == "" || rel == "." {
		return "", nil
	}
	if filepath.IsAbs(rel) || hasVolumeName(rel) {
		return "", fmt.Errorf("entry %q escapes destination directory", name)
	}

	joined := filepath.Join(dest, rel)

	absDest, err := filepath.Abs(dest)
	if err != nil {
		return "", fmt.Errorf("resolving destination: %w", err)
	}
	absJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", rel, err)
	}

	relToDest, err := filepath.Rel(absDest, absJoined)
	if err != nil {
		return "", fmt.Errorf("entry %q escapes destination directory", name)
	}
	// Normalize to '/' so the escape check is portable across OS separators.
	relToDest = filepath.ToSlash(relToDest)
	if relToDest == ".." || strings.HasPrefix(relToDest, "../") {
		return "", fmt.Errorf("entry %q escapes destination directory", name)
	}

	return joined, nil
}

// hasVolumeName reports whether path begins with a Windows volume name.
func hasVolumeName(path string) bool {
	if len(path) < 2 {
		return false
	}
	return path[1] == ':'
}
