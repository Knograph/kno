// Command kno measures which data assets earn their place in an LLM agent.
package main

import (
	"context"
	"os"

	"github.com/knograph/kno/cli"
)

func main() {
	os.Exit(cli.Execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
