package config

import _ "embed"

// example-config.toml is the complete, commented reference configuration. It
// is embedded into the binary so `garess init` can write a starter config
// even when the example file is not present on disk (release binaries ship
// as single files). Keep it in sync with the defaults in config.go; the parse
// test in example_test.go guards against drift.
//
//go:embed example-config.toml
var exampleConfig []byte

// Example returns the bundled example-config.toml template: a fully
// commented starter config with live [[providers]] blocks and guides for
// hooks, the sandbox and MCP servers.
func Example() []byte {
	return exampleConfig
}
