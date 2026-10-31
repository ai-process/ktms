//go:build !((darwin && cgo) || linux)

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "ktms-node-tray runs on macOS (built with cgo) and Linux")
	os.Exit(1)
}
