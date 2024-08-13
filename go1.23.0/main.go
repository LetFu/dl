// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The go1.23.0 command runs the go command from Go 1.23.0.
//
// To install, run:
//
//	$ go install golang.org/dl/go1.23.0@latest
//	$ go1.23.0 download
//
// And then use the go1.23.0 command as if it were your normal go
// command.
//
// See the release notes at https://go.dev/doc/go1.23.
//
// File bugs at https://go.dev/issue/new.
package main

import "golang.org/dl/internal/version"

func main() {
	version.Run("go1.23.0")
}
