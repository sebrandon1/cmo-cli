package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the cmo version",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if structured() {
			return printStructured(cmd.OutOrStdout(), map[string]string{"version": rootCmd.Version})
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "cmo version %s\n", rootCmd.Version)
		return err
	},
}

func init() { rootCmd.AddCommand(versionCmd) }
