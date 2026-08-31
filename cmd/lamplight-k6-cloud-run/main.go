package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"lamplight/internal/k6cloudrun"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, err := k6cloudrun.New(ctx)
	if err == nil {
		err = k6cloudrun.RunTask(ctx, client)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lamplight k6 task failed:", err)
		os.Exit(1)
	}
}
