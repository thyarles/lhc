// Command lhc is the Linux Health Check.
package main

import (
	"os"

	"github.com/thyarles/lhc/internal/cli"
)

func main() { os.Exit(cli.Execute()) }
