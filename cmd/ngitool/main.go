// Command ngitool manages every nginx on a server. Wiring only; the commands
// live in internal/cli.
package main

import (
	"os"

	"github.com/amirhosseinbanaei/NgiTool/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
