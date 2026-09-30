package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
	"github.com/amirhosseinbanaei/NgiTool/internal/version"
)

func versionCmd(e *env) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:         "version",
		Short:       "version, commit, build date, Go, os/arch",
		Annotations: map[string]string{annGroup: "tool", annSynopsis: "version [--json]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := version.Get()
			if asJSON {
				return printJSON(info)
			}
			ui.Plan("NgiTool "+info.Version, [][2]string{
				{"commit", info.Commit},
				{"built", info.Date},
				{"go", info.Go},
				{"platform", info.OS + "/" + info.Arch},
			})
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(ui.Out, string(b))
	return err
}
