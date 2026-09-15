package main

import (
	"fmt"
	"os"

	"github.com/jphenow/sp/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		// The root command sets SilenceErrors so cobra doesn't print it, which
		// makes printing it HERE mandatory — without this, every failure exits
		// 1 with no output at all and the user just sees their prompt come
		// back. Silent failure is worse than any error text.
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
