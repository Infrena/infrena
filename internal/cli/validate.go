package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newValidateCommand(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check configuration for errors without contacting providers",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("validate is implemented in Task 14")
		},
	}
}
