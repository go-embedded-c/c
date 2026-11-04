// Command vendorcc regenerates internal/cc, the patched copy of
// modernc.org/cc/v4 the front end parses with, from that module's directory
// in the module cache.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// osExit is replaced by tests.
var osExit = os.Exit

func main() {
	if err := RootCmd().Execute(); err != nil {
		osExit(1)
	}
}

// RootCmd builds the command.
func RootCmd() *cobra.Command {
	var out, src string
	cmd := &cobra.Command{
		Use:          "vendorcc",
		Short:        "copy modernc.org/cc/v4 " + Version + " into internal/cc, patched",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := Generate(out, src); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", filepath.Join("internal", "cc"), "destination directory (replaced)")
	cmd.Flags().StringVar(&src, "cc", "", "the module directory, $(go env GOMODCACHE)/modernc.org/cc/v4@"+Version)
	_ = cmd.MarkFlagRequired("cc")
	return cmd
}
