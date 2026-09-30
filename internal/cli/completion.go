package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

var shells = []ui.Option{
	{Value: "bash", Label: "bash", Hint: "/etc/bash_completion.d/ngitool"},
	{Value: "zsh", Label: "zsh", Hint: "a directory on $fpath, as _ngitool"},
	{Value: "fish", Label: "fish", Hint: "~/.config/fish/completions/ngitool.fish"},
}

func completionCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "completion",
		Short: "shell completion for bash, zsh or fish",
		Long: `Prints the completion script for a shell. Without an argument on a terminal it
asks which shell and shows how to install it.`,
		Example: `ngitool completion bash > /etc/bash_completion.d/ngitool
ngitool completion zsh > "${fpath[1]}/_ngitool"
ngitool completion fish > ~/.config/fish/completions/ngitool.fish`,
		Annotations: map[string]string{annGroup: "tool", annSynopsis: "completion bash|zsh|fish"},
		Args:        maxArgs(1),
		ValidArgs:   []string{"bash", "zsh", "fish"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return writeCompletion(e.root, args[0])
			}
			if err := ui.Need("shell", "bash|zsh|fish as the argument"); err != nil {
				return err
			}
			shell, err := ui.Select(ui.SelectOpts{Title: "Which shell?", Options: shells})
			if err != nil {
				return err
			}
			var hint string
			for _, s := range shells {
				if s.Value == shell {
					hint = s.Hint
				}
			}
			ui.Plan("Install "+shell+" completion", [][2]string{
				{"run", ui.Key("ngitool completion " + shell + " > " + hint)},
				{"then", "open a new shell"},
			})
			return nil
		},
	}
}

func writeCompletion(root *cobra.Command, shell string) error {
	switch shell {
	case "bash":
		return root.GenBashCompletionV2(ui.Out, true)
	case "zsh":
		return root.GenZshCompletion(ui.Out)
	case "fish":
		return root.GenFishCompletion(ui.Out, true)
	}
	return &UsageError{Msg: fmt.Sprintf("unsupported shell %q (bash, zsh or fish)", shell), Cmd: "ngitool completion"}
}
