# cpio

[![Go Reference](https://pkg.go.dev/badge/github.com/unikraft/go-cpio.svg)](https://pkg.go.dev/github.com/unikraft/go-cpio) [![Build Status](https://app.travis-ci.com/unikraft/go-cpio.svg?branch=main)](https://app.travis-ci.com/unikraft/go-cpio) [![Go Report Card](https://goreportcard.com/badge/github.com/unikraft/go-cpio)](https://goreportcard.com/report/github.com/unikraft/go-cpio)

Package cpio provides readers and writers for the CPIO archive file format.

Currently, only the SVR4 (New ASCII) format is supported, both with and without
checksums.

Use `cpio.OpenFS` to read the files of an archive through the `io/fs`
interfaces.

Use `cpio.Create` to write the files of an `fs.FS` to an archive. CPIO cannot
keep extended attributes, thus `cpio.Create` does not write them.

Copyright 2021, Ryan Armstrong <ryan@cavaliercoder.com>
