// Package all links every first-party module into a binary. A distribution
// is just a main package that imports the modules it ships.
package all

import (
	_ "github.com/a5anka/amp-modular-poc/modules/aigateway"
	_ "github.com/a5anka/amp-modular-poc/modules/evaluation"
	_ "github.com/a5anka/amp-modular-poc/modules/observability"
)
