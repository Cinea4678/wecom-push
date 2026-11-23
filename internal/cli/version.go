package cli

import (
	"fmt"

	"github.com/Cinea4678/wecom-push/internal/version"
	"github.com/spf13/cobra"
)

func newVersionCmd(opts *GlobalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and build metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.JSON {
				return printJSON(cmd.OutOrStdout(), map[string]string{
					"version": version.Version,
					"commit":  version.Commit,
					"date":    version.Date,
				})
			}
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return nil
		},
	}
}
