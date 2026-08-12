package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/gosuda/cohotfs/internal/cli"
	"github.com/gosuda/cohotfs/internal/hostroot"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	code := cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr, cli.Dependencies{OpenRoot: hostroot.Open})
	os.Exit(code)
}
