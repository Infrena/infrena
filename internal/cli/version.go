package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/planner"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/internal/version"
	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/report"
)

// newVersionCommand reports what this binary is and which formats it speaks.
// PLAN.md §61.4.
//
// The formats matter more than the version number. "Which version do I need" is
// otherwise answerable only by reading source, and a bug report that says "0.4.1"
// does not say whether that build can read the state file attached to it.
func newVersionCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:           "version",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Report this build and the formats it understands",
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := version.Describe(formats())
			if opts.Output == "" {
				renderVersion(cmd.OutOrStdout(), info)
				return nil
			}
			// --output writes the same thing as one JSON object, so a CI job can
			// assert on it rather than parsing the text.
			return writeVersionJSON(opts.Output, info)
		},
	}
}

// formats lists every versioned wire format, reading each from the package that owns
// it rather than restating the number — so a bump cannot be made without this
// following.
//
// ASSEMBLED HERE because internal/version may not import these: the compiler imports
// it to check a project's `infrena:` floor, and a Formats() that read
// planner.PlanVersion would close an import cycle.
func formats() []version.Format {
	return []version.Format{
		{Name: "state", Versions: []int{state.CurrentVersion}},
		// A SET, and the only one today: the handshake negotiates, so a build can
		// keep talking to plugins written against an older protocol.
		{Name: "plugin protocol", Versions: append([]int(nil), pluginproto.Supported...)},
		{Name: "plan artifact", Versions: []int{planner.PlanVersion}},
		{Name: "report", Versions: []int{report.Version}},
	}
}

func renderVersion(w io.Writer, info version.Info) {
	fmt.Fprintf(w, "infrena %s (%s)\n", info.Version, strings.Join(nonEmpty(
		info.Revision, info.Go, info.Platform), ", "))

	if len(info.Formats) == 0 {
		return
	}
	width := 0
	for _, f := range info.Formats {
		if len(f.Name) > width {
			width = len(f.Name)
		}
	}
	fmt.Fprintf(w, "\nformats\n")
	for _, f := range info.Formats {
		fmt.Fprintf(w, "  %-*s  %s\n", width, f.Name, joinInts(f.Versions))
	}
}

func writeVersionJSON(path string, info version.Info) error {
	body, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	// 0600, matching the plan artifact and reports. Nothing secret is in here, but
	// one permission for everything this tool writes is a rule worth not making
	// exceptions to.
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

func nonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinInts(ns []int) string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return strings.Join(out, ", ")
}
