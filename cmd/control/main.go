// Command control runs the hmoob-dock control端.
//
// The control端 is a single instance service: it owns the SQLite database,
// the master key and the HTTP API. It never prints credentials or the
// content of the configuration to the log.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
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
	// stateTimeout bounds the read of the state of the administrator.
	stateTimeout = 5 * time.Second
	// healthCheckTimeout bounds the probe of the health endpoint.
	healthCheckTimeout = 4 * time.Second
	// healthCheckPath is the path of the liveness endpoint of the control端.
	healthCheckPath = "/healthz"
)

// The flags of the command.
//
// The flags live in the package, so the health check of the image and the
// tests of the command see the same set of flags.
var (
	// addrFlag is the listen address of the control端.
	addrFlag = flag.String("addr", "", "listen address of the control端")
	// genKeyFlag is the file that receives a new master key.
	genKeyFlag = flag.String("gen-key", "", "write a new master key file to the given path and exit")
	// healthCheckFlag turns the command into a probe of its own health
	// endpoint.
	healthCheckFlag = flag.Bool("health-check", false, "probe the health endpoint of the control端 and exit")
)

// initializationState is implemented by the handler of the control端. It
// reports whether the administrator exists already.
type initializationState interface {
	Initialized(context.Context) (bool, error)
}

// listenAddress returns the address the control端 listens on.
//
// The rule is the same for the server and for its health check: an address
// that the operator names wins, otherwise the address follows the switch of
// TLS. A deployment with a certificate answers HTTPS on 8443, a deployment
// without one answers HTTP on 8080.
func listenAddress(addr string, cfg config.Control) string {
	if addr != "" {
		return addr
	}
	if cfg.TLSCert != "" {
		return defaultTLSAddress
	}
	return defaultPlainAddress
}

// healthTarget describes the endpoint the health check asks.
type healthTarget struct {
	// scheme is "http" or "https".
	scheme string
	// address is the host and the port of the probe.
	address string
}

// url returns the address of the probe.
func (t healthTarget) url() string {
	return t.scheme + "://" + t.address + healthCheckPath
}

// healthTargetFor returns the endpoint the health check has to ask.
//
// The target follows the listener of the control端: the same address and the
// same switch of TLS. A wildcard address names every address of the host,
// so it is asked on the loopback, which the container reaches.
func healthTargetFor(addr string, cfg config.Control) (healthTarget, error) {
	listen := listenAddress(addr, cfg)

	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return healthTarget{}, fmt.Errorf("the listen address %s is malformed: %w", listen, err)
	}
	if port == "" {
		return healthTarget{}, fmt.Errorf("the listen address %s names no port", listen)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}

	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	return healthTarget{scheme: scheme, address: net.JoinHostPort(host, port)}, nil
}

// probeHealth asks the health endpoint of the control端 whether it answers.
//
// The certificate of the deployment is not verified: the check proves that
// the process answers and says nothing about the trust of the certificate.
func probeHealth(target healthTarget) error {
	client := &http.Client{
		Timeout: healthCheckTimeout,
		Transport: &http.Transport{
			// The check proves the answer of the process, not the trust of
			// the certificate of the deployment.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	response, err := client.Get(target.url())
	if err != nil {
		return fmt.Errorf("ask %s: %w", target.url(), err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered with the status %d", target.url(), response.StatusCode)
	}
	return nil
}

// runHealthCheck asks the running control端 for its health and reports
// whether it answers.
//
// The command reads the same configuration as the server and resolves the
// address of the probe with the same rule, so the check asks the listener
// that the deployment really opens.
func runHealthCheck(addr string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	target, err := healthTargetFor(addr, cfg)
	if err != nil {
		return err
	}
	if err := probeHealth(target); err != nil {
		return err
	}
	log.Printf("control: the health check answered %s", target.url())
	return nil
}

func main() {
	flag.Parse()

	if *genKeyFlag != "" {
		if err := config.GenerateMasterKey(*genKeyFlag); err != nil {
			log.Fatalf("control: %v", err)
		}
		log.Printf("control: a new master key was written to %s", *genKeyFlag)
		return
	}

	// The health check of the image runs the command a second time and asks
	// the running control端 for its health. It never starts a server.
	if *healthCheckFlag {
		if err := runHealthCheck(*addrFlag); err != nil {
			log.Fatalf("control: the health check failed: %v", err)
		}
		return
	}

	if err := run(*addrFlag); err != nil {
		log.Fatalf("control: %v", err)
	}
}

// newHandler combines the API of the control端 with its frontend.
//
// The API lives below /api/v1 and answers the two health endpoints, so
// every other path belongs to the frontend. The frontend answers the
// navigation of the application with the index of its build and never
// answers a path of the API.
func newHandler(api http.Handler, web http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	mux.Handle("/healthz", api)
	mux.Handle("/readyz", api)
	mux.Handle("/", web)
	return mux
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

	// The frontend is optional: the API of the control端 works without it.
	// A frontend that was configured but cannot be read is reported, so a
	// misconfigured deployment does not hide behind a blank page.
	if err := cfg.CheckWebDir(); err != nil {
		log.Printf("control: %v", err)
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

	// The state of the administrator is read from the service of the
	// control端. The bootstrap credential was already read and stored by
	// the server, so it is not read a second time here and its content
	// never reaches the log.
	if state, ok := handler.(initializationState); ok {
		ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
		defer cancel()
		initialized, err := state.Initialized(ctx)
		if err != nil {
			return fmt.Errorf("read the state of the administrator: %w", err)
		}
		if initialized {
			log.Printf("control: the administrator is initialized")
		} else {
			log.Printf("control: the administrator is not initialized yet")
		}
	}

	// The address of the listener is resolved by the same rule as the
	// health check of the image uses, so the check asks the server that
	// runs.
	addr = listenAddress(addr, cfg)

	// The frontend and the API share one origin, so the browser needs no
	// second server and no cross origin request.
	router := newHandler(handler, httpapi.StaticHandler(cfg.WebDir))

	server := &http.Server{
		Addr:              addr,
		Handler:           router,
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
