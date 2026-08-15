package main

import (
	"context"
	"os"

	"github.com/yty-build/acp-gateway/internal/buildinfo"
	"github.com/yty-build/acp-gateway/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, buildinfo.Current()))
}
