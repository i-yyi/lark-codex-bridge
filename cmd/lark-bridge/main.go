package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	configPath := flag.String("config", "config.json", "path to JSON config")
	checkConfig := flag.Bool("check-config", false, "validate config and exit")
	probeLark := flag.Bool("probe-lark", false, "send probe messages to validate Lark bot permissions and exit")
	healthOnce := flag.Bool("health-once", false, "send one health check message and exit")
	flag.Parse()

	logger := log.New(os.Stdout, "lark-bridge ", log.LstdFlags|log.Lmicroseconds)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *checkConfig {
		if err := runConfigCheck(*configPath, logger); err != nil {
			logger.Fatal(err)
		}
		return
	}

	if *probeLark {
		if err := runLarkProbe(ctx, *configPath, logger); err != nil && !errors.Is(err, context.Canceled) {
			logger.Fatal(err)
		}
		return
	}

	if *healthOnce {
		if err := runHealthOnce(ctx, *configPath, logger); err != nil && !errors.Is(err, context.Canceled) {
			logger.Fatal(err)
		}
		return
	}

	if err := run(ctx, *configPath, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Fatal(err)
	}
}
