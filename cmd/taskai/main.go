// Command taskai starts the TaskAI terminal application.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/yty-build/acp-gateway/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(app.Run(ctx, os.Args[1:], app.IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
}
