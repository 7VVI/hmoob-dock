// Command control runs the hmoob-dock control端.
//
// The control端 is a single instance service: it owns the SQLite database,
// the master key and the HTTP API. It never prints credentials or the
// content of the configuration to the log.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"hmoob-dock/internal/config"
	"hmoob-dock/internal/httpapi"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

const (
	// defaultPlainAddress is the listen address used without TLS.
	defaultPlainAddress = ":8080"
	// defaultTLSAddress is the listen address used with TLS.
	defaultTLSAddress = ":8443"
	// shutdownTimeout bounds the graceful shutdown.
	shutdownTimeout = 10 * time.Second
	// readHeaderTimeout bounds how long a client may take to send the
	// request headers.
	readHeaderTimeout = 10 * time.Second
)

func main() {
	genKey := flag.String("gen-key", "", "write a new master key file to the given path and exit")
	addr := flag.String("addr", "", "listen address of the control端")
	flag.Parse()

	if *genKey != "" {
		if err := config.GenerateMasterKey(*genKey); err != nil {
			log.Fatalf("control: %v", err)
		}
		log.Printf("control: a new master key was written to %s", *genKey)
		return
	}

	if err := run(*addr); err != nil {
		log.Fatalf("control: %v", err)
	}
}

// run starts the control端 and blocks until it is asked to stop.
func run(addr string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return fmt.Errorf("create the database directory: %w", err)
	}

	key, err := config.LoadMasterKey(cfg.KeyFile)
	if err != nil {
		return err
	}
	box, err := secret.New(key)
	if err != nil {
		return err
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("control: close the database: %v", err)
		}
	}()

	handler, err := httpapi.NewServer(db, box, cfg)
	if err != nil {
		return err
	}
	if closer, ok := handler.(io.Closer); ok {
		defer func() {
			if err := closer.Close(); err != nil {
				log.Printf("control: close the read pool: %v", err)
			}
		}()
	}

	if cfg.BootstrapFile != "" {
		// The bootstrap credential is only read here. It is consumed by the
		// administrator initialization, and its content never reaches the
		// log.
		token, err := config.LoadBootstrapToken(cfg.BootstrapFile)
		if err != nil {
			return err
		}
		if token != "" {
			log.Printf("control: the bootstrap credential from %s is loaded and not consumed yet", cfg.BootstrapFile)
		}
	}

	if addr == "" {
		addr = defaultPlainAddress
		if cfg.TLSCert != "" {
			addr = defaultTLSAddress
		}
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	failures := make(chan error, 1)
	go func() {
		log.Printf("control: listening on %s, database %s", addr, cfg.DBPath)
		if cfg.TLSCert != "" {
			failures <- server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
			return
		}
		failures <- server.ListenAndServe()
	}()

	select {
	case err := <-failures:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Printf("control: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
