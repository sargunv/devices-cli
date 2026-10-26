// Command devices-cli gives each git worktree its own ephemeral test device.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/sargunv/devices-cli/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
