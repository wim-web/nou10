package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/wim-web/nou10/internal/cli"
)

var version = Version

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, version); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "nou10:", err)
		os.Exit(1)
	}
}
