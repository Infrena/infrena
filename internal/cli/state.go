package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newStateCommand(opts *GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Inspect and manage recorded state",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List managed resources",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("state list is implemented in Task 14")
		},
	})
	return cmd
}
