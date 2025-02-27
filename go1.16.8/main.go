// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The go1.16.8 command runs the go command from Go 1.16.8.
//
// To install, run:
//
//	$ go install github.com/LetFu/dl/go1.16.8@latest
//	$ go1.16.8 download
//
// And then use the go1.16.8 command as if it were your normal go
// command.
//
// See the release notes at https://github.com/LetFu/doc/devel/release.html#go1.16.minor
//
// File bugs at https://github.com/LetFu/issues/new
package main

import "github.com/LetFu/dl/internal/version"

func main() {
	version.Run("go1.16.8")
}
