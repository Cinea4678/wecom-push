package main

import (
	"os"

	"github.com/Cinea4678/wecom-push/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
