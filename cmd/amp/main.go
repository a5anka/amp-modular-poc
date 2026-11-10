// Command amp is the full distribution: the core plus every first-party module.
// Linking a module does not install it; `amp module install <name>` does.
package main

import (
	"github.com/a5anka/amp-modular-poc/internal/cli"
	_ "github.com/a5anka/amp-modular-poc/modules/all"
)

func main() { cli.Main("amp (full distribution)") }
