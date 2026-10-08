package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mydevmachine/session-share/internal/cli"
)

var version = "dev"

func main() {
	c, err := cli.New(version, os.Stdout, os.Stderr)
	if err == nil {
		err = c.Run(context.Background(), os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "session-share:", err)
		os.Exit(1)
	}
}
