// Command helper is one process of a test process tree (see package
// attesttest). Tests build it once and copy it to the paths that play the
// host app, the provider and the client; it is small, so a tree starts fast
// even when the tests themselves run under the race detector.
package main

import (
	"os"

	"github.com/williamsmiths/kongcowork-harness/internal/attest/attesttest"
)

func main() { os.Exit(attesttest.Level()) }
