// Package pbp holds files from the repository root that the binary embeds.
package pbp

import _ "embed"

// ExampleConfig is config.example.yaml, the template `pbp setup` writes from.
//
//go:embed config.example.yaml
var ExampleConfig []byte
