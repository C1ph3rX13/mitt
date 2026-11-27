package main

import (
	"os"

	"github.com/C1ph3rX13/mitt/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Stdout, os.Stderr))
}
