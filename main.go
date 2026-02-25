package main

import (
	"os"

	"github.com/jphenow/sp/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
