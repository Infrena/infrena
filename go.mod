module github.com/infrena/infrena

// The minimum Go a consumer needs. Raised to 1.27 on 2026-09-13, deliberately and at
// the owner's direction.
//
// This line is part of a public contract: pkg/* is compiled by plugin authors in their
// own modules (PLAN.md §31.1), so raising it makes THEIR build fail, not ours — a
// plugin repository must now also declare 1.27. The trade taken is that the project
// tracks the current toolchain rather than supporting older ones, which is the right
// call while the protocol is young and the only plugin is developed alongside it.
//
// It now matches mise.toml, so CI has one meaningful toolchain rather than a floor and
// a current. Drop this below mise.toml's pin and the floor matrix entry earns its place
// again — see .github/workflows/ci.yml.
go 1.27.0

require (
	github.com/spf13/cobra v1.10.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
)
