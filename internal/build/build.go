// SPDX-License-Identifier: Apache-2.0

// Package build carries what this binary knows about itself.
package build

// Version is the release this binary was built from, set at link time:
//
//	go build -ldflags="-X github.com/openpreflight/openpreflight/internal/build.Version=2.1.0"
//
// It stays "dev" for a local `go build` and for tests, which is the honest
// answer — an unversioned build is not 2.1.0 just because the tree is.
var Version = "dev"
