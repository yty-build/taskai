// Command taskai starts the TaskAI terminal application.
package main

import (
	"os"

	"github.com/yty-build/acp-gateway/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:], os.Stdout, os.Stderr))
}
