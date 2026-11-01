// Command genheaders regenerates headers/include from the oracle toolchain
// ($CORACLE_HOME, see internal/oracle) and the licence files of the pinned
// llvm-project and wasi-libc checkouts.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/go-embedded-c/c/internal/oracle"
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
	var out, llvm, wasi string
	cmd := &cobra.Command{
		Use:          "genheaders",
		Short:        "copy the oracle's wasm32-wasip1 headers into headers/include",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tc, err := oracle.Locate()
			if err != nil {
				return err
			}
			if err := tc.Verify(cmd.Context()); err != nil {
				return err
			}
			res, err := tc.ResourceInclude(cmd.Context())
			if err != nil {
				return err
			}
			err = Generate(out, Sources{
				ClangResourceInclude: res,
				WasiInclude:          filepath.Join(tc.Sysroot, "include", oracle.Target),
				LLVMLicense:          filepath.Join(llvm, "clang", "LICENSE.TXT"),
				WasiLicenses:         wasi,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", filepath.Join("headers", "include"), "destination directory (replaced)")
	cmd.Flags().StringVar(&llvm, "llvm", "", "llvm-project checkout at llvmorg-"+oracle.ClangVersion+" (for its licence)")
	cmd.Flags().StringVar(&wasi, "wasi-libc", "", "wasi-libc checkout at "+oracle.WasiLibcCommit[:12]+" (for its licences)")
	_ = cmd.MarkFlagRequired("llvm")
	_ = cmd.MarkFlagRequired("wasi-libc")
	return cmd
}
