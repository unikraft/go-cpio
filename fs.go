// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, Unikraft GmbH.
// Licensed under the BSD-3-Clause License (the "License").
// You may not use this file except in compliance with the License.

package cpio

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
)

var (
	_ fs.FS         = (*FS)(nil)
	_ fs.ReadDirFS  = (*FS)(nil)
	_ fs.ReadFileFS = (*FS)(nil)
	_ fs.StatFS     = (*FS)(nil)
	_ fs.ReadLinkFS = (*FS)(nil)
)

// maxSymlinks is the maximum number of symbolic links that one lookup follows.
const maxSymlinks = 40

var (
	errNotDir       = errors.New("not a directory")
	errIsDir        = errors.New("is a directory")
	errTooManyLinks = errors.New("too many levels of symbolic links")
)

// FS is a read-only file system for the files in a CPIO archive.
type FS struct {
	ra   io.ReaderAt
	root *dirent
}

// OpenFS reads the headers of the SVR4 CPIO archive in ra. The returned FS
// reads the file data from ra when necessary, thus ra must stay open.
func OpenFS(ra io.ReaderAt) (*FS, error) {
	fsys := &FS{
		ra:   ra,
		root: &dirent{hdr: Header{Name: ".", Mode: TypeDir | 0o755}},
	}

	r := &offsetReader{ra: ra}
	for seq := 0; ; seq++ {
		hdr, _, err := readSVR4Header(r)
		if err == io.EOF {
			// A header is returned only for the trailer.
			if hdr == nil {
				return nil, io.ErrUnexpectedEOF
			}
			break
		}
		if err != nil {
			return nil, err
		}

		d := &dirent{hdr: *hdr, off: r.off, seq: seq}
		r.off += hdr.Size + hdr.pad
		if err := fsys.insert(d); err != nil {
			return nil, err
		}
	}

	fsys.link()

	return fsys, nil
}

// Open opens the named file. It follows symbolic links.
func (fsys *FS) Open(name string) (fs.File, error) {
	d, err := fsys.find("open", name, true)
	if err != nil {
		return nil, err
	}

	if d.IsDir() {
		return &dirFile{name: name, info: d.info(name), entries: d.entries()}, nil
	}

	return &file{
		SectionReader: io.NewSectionReader(fsys.ra, d.off, d.hdr.Size),
		info:          d.info(name),
	}, nil
}

// ReadDir returns the entries of the named directory, sorted by name.
func (fsys *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	d, err := fsys.find("readdir", name, true)
	if err != nil {
		return nil, err
	}

	if !d.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errNotDir}
	}

	return d.entries(), nil
}

// ReadFile returns the contents of the named file.
func (fsys *FS) ReadFile(name string) ([]byte, error) {
	d, err := fsys.find("read", name, true)
	if err != nil {
		return nil, err
	}

	if d.IsDir() {
		return nil, &fs.PathError{Op: "read", Path: name, Err: errIsDir}
	}

	b := make([]byte, d.hdr.Size)
	if _, err := io.ReadFull(io.NewSectionReader(fsys.ra, d.off, d.hdr.Size), b); err != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: err}
	}

	return b, nil
}

// Stat returns the file information of the named file. It follows symbolic
// links.
func (fsys *FS) Stat(name string) (fs.FileInfo, error) {
	d, err := fsys.find("stat", name, true)
	if err != nil {
		return nil, err
	}

	return d.info(name), nil
}

// Lstat returns the file information of the named file. It does not follow a
// symbolic link at the end of name.
func (fsys *FS) Lstat(name string) (fs.FileInfo, error) {
	d, err := fsys.find("lstat", name, false)
	if err != nil {
		return nil, err
	}

	return d.info(name), nil
}

// ReadLink returns the target of the named symbolic link.
func (fsys *FS) ReadLink(name string) (string, error) {
	d, err := fsys.find("readlink", name, false)
	if err != nil {
		return "", err
	}

	if d.hdr.Mode&ModeType != TypeSymlink {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
	}

	return d.hdr.Linkname, nil
}

// find makes sure that name is valid and finds its file. It puts errors in an
// fs.PathError for op.
func (fsys *FS) find(op, name string, follow bool) (*dirent, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}

	d, err := fsys.lookup(name, follow)
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}

	return d, nil
}

// lookup finds the file at name. It follows symbolic links in the parent
// directories, and at the end of name only if follow is true.
func (fsys *FS) lookup(name string, follow bool) (*dirent, error) {
	hops := 0
	d := fsys.root

	rest := name
	if rest == "." {
		rest = ""
	}

	for rest != "" {
		if !d.IsDir() {
			return nil, errNotDir
		}

		var elem string
		elem, rest, _ = strings.Cut(rest, "/")

		c, ok := d.children[elem]
		if !ok {
			return nil, fs.ErrNotExist
		}

		if c.hdr.Mode&ModeType == TypeSymlink && (rest != "" || follow) {
			hops++
			if hops > maxSymlinks {
				return nil, errTooManyLinks
			}

			// Start again from the root with the target and the remaining elements.
			target := c.hdr.Linkname
			if !path.IsAbs(target) {
				target = path.Join(d.hdr.Name, target)
			}
			rest = path.Join(cleanPath(target), rest)
			d = fsys.root
			continue
		}

		d = c
	}

	return d, nil
}

// insert puts d in the tree at its name. It makes the parent directories that
// are not in the archive.
func (fsys *FS) insert(d *dirent) error {
	name := cleanPath(d.hdr.Name)
	if name == "" {
		if d.IsDir() {
			d.hdr.Name = "."
			fsys.root.hdr = d.hdr
		}
		return nil
	}

	parent, err := fsys.mkdirAll(path.Dir(name))
	if err != nil {
		return fmt.Errorf("cpio: %s: %w", d.hdr.Name, err)
	}

	elem := path.Base(name)
	d.hdr.Name = path.Join(parent.hdr.Name, elem)
	parent.add(elem, d)

	return nil
}

// mkdirAll returns the directory at dir. It makes the directories that do not
// exist, and follows symbolic links.
func (fsys *FS) mkdirAll(dir string) (*dirent, error) {
	d := fsys.root
	if dir == "." {
		return d, nil
	}

	for elem := range strings.SplitSeq(dir, "/") {
		c, ok := d.children[elem]
		if !ok {
			c = &dirent{hdr: Header{
				Name: path.Join(d.hdr.Name, elem),
				Mode: TypeDir | 0o755,
			}}
			d.add(elem, c)
		}

		if c.hdr.Mode&ModeType == TypeSymlink {
			var err error
			if c, err = fsys.lookup(c.hdr.Name, true); err != nil {
				return nil, err
			}
		}

		if !c.IsDir() {
			return nil, errNotDir
		}

		d = c
	}

	return d, nil
}

// linkKey identifies the entries of one hard link group, as the Linux
// initramfs unpacker does.
type linkKey struct {
	ino          int64
	major, minor int
	mode         FileMode
}

// linkGroup is the inode and the entries of one hard link group.
type linkGroup struct {
	ino     int64
	members []*dirent
}

// link gives inode numbers in path order. All entries of a hard link group
// get one inode and the data of the last entry that has data.
func (fsys *FS) link() {
	var ino int64
	var groups []*linkGroup
	byKey := map[linkKey]*linkGroup{}

	fsys.root.walk(func(d *dirent) {
		t := d.hdr.Mode & ModeType
		if d.hdr.Links < 2 || t == TypeDir || t == TypeSymlink {
			ino++
			d.hdr.Inode = ino
			d.hdr.Links = 1
			return
		}

		key := linkKey{d.hdr.Inode, d.hdr.DevMajor, d.hdr.DevMinor, d.hdr.Mode}
		g, ok := byKey[key]
		if !ok {
			ino++
			g = &linkGroup{ino: ino}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.members = append(g.members, d)
	})

	for _, g := range groups {
		src := g.members[0]
		for _, m := range g.members {
			if m.hdr.Size > 0 && (src.hdr.Size == 0 || m.seq > src.seq) {
				src = m
			}
		}

		for _, m := range g.members {
			name := m.hdr.Name
			m.hdr = src.hdr
			m.off = src.off
			m.hdr.Name = name
			m.hdr.Inode = g.ino
			m.hdr.Links = len(g.members)
		}
	}
}

// cleanPath returns name relative to the root. It returns "" for the root.
func cleanPath(name string) string {
	return strings.TrimPrefix(path.Clean("/"+name), "/")
}

// dirent is a file in the tree of an FS.
type dirent struct {
	hdr      Header // Name is the path in the tree
	off      int64  // offset of the file data in the archive
	seq      int    // index of the entry in the archive
	children map[string]*dirent
}

var _ fs.DirEntry = (*dirent)(nil)

func (d *dirent) Name() string               { return path.Base(d.hdr.Name) }
func (d *dirent) IsDir() bool                { return d.hdr.Mode.IsDir() }
func (d *dirent) Type() fs.FileMode          { return d.hdr.FileInfo().Mode().Type() }
func (d *dirent) Info() (fs.FileInfo, error) { return d.info(d.hdr.Name), nil }

// info returns the file information of d, with name as the path.
func (d *dirent) info(name string) fs.FileInfo {
	h := d.hdr
	h.Name = name
	return h.FileInfo()
}

// add puts c in d as elem. A directory that replaces a directory keeps its
// children.
func (d *dirent) add(elem string, c *dirent) {
	if old, ok := d.children[elem]; ok && old.IsDir() && c.IsDir() {
		old.hdr = c.hdr
		return
	}

	if d.children == nil {
		d.children = map[string]*dirent{}
	}
	d.children[elem] = c
}

// entries returns the children of d, sorted by name.
func (d *dirent) entries() []fs.DirEntry {
	names := slices.Sorted(maps.Keys(d.children))
	list := make([]fs.DirEntry, len(names))
	for i, name := range names {
		list[i] = d.children[name]
	}
	return list
}

// walk calls fn for d and then for all files below d, in path order.
func (d *dirent) walk(fn func(*dirent)) {
	fn(d)
	for _, name := range slices.Sorted(maps.Keys(d.children)) {
		d.children[name].walk(fn)
	}
}

// file is an open file that is not a directory.
type file struct {
	*io.SectionReader
	info fs.FileInfo
}

func (f *file) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *file) Close() error               { return nil }

// dirFile is an open directory.
type dirFile struct {
	name    string
	info    fs.FileInfo
	entries []fs.DirEntry
	pos     int
}

func (f *dirFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *dirFile) Close() error               { return nil }

func (f *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: f.name, Err: errIsDir}
}

// ReadDir returns the next n entries of the directory, or all of the remaining
// entries if n <= 0.
func (f *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := f.entries[f.pos:]
	if n <= 0 {
		f.pos = len(f.entries)
		return rest, nil
	}

	if len(rest) == 0 {
		return nil, io.EOF
	}

	n = min(n, len(rest))
	f.pos += n

	return rest[:n], nil
}

// offsetReader reads from an io.ReaderAt and keeps the offset of the next read.
type offsetReader struct {
	ra  io.ReaderAt
	off int64
}

func (r *offsetReader) Read(p []byte) (int, error) {
	n, err := r.ra.ReadAt(p, r.off)
	r.off += int64(n)
	return n, err
}
