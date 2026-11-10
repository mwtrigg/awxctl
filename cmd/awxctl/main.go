package main

import (
	"os"

	"github.com/mwtrigg/awxctl/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
