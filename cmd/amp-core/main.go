// Command amp-core is the core with no modules linked. Useful to show that
// the core runs on its own, and that a binary can only install what it links.
package main

import "github.com/a5anka/amp-modular-poc/internal/cli"

func main() { cli.Main("amp-core (core only)") }
