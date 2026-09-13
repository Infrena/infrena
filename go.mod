module github.com/infrata/infrata

// The MINIMUM Go a consumer needs, not the toolchain this is developed with — that
// is mise.toml, currently 1.27. They are deliberately different: pkg/* is compiled by
// plugin authors in their own modules (PLAN.md §31.1), so this line is part of a public
// contract and raising it makes their build fail, not ours.
//
// 1.24 rather than 1.24.13, which needlessly refused every 1.24 patch before .13. What
// holds it at 1.24 at all is strings.SplitSeq, introduced there; the language-version
// check enforces the floor at compile time, so an accidental use of something newer
// fails here rather than on a user's machine.
go 1.24.0

require (
	github.com/spf13/cobra v1.10.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
)
