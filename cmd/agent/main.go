// Command agent runs the hmoob-dock agent of a node.
//
// The agent connects to the control端 and reports the state of its node. It
// never prints a credential, it never opens a port of its own and it never
// connects to anything but the control端.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"hmoob-dock/internal/agent"
	"hmoob-dock/internal/config"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("agent: %v", err)
	}
}

// run starts the agent and blocks until it is asked to stop.
func run() error {
	cfg, err := config.LoadAgent()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("agent: connecting to the control端 %s", cfg.ServerURL)
	return agent.Run(ctx, cfg)
}
