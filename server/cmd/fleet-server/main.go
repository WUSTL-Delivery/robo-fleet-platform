// fleet-server: the control server. One process, one box (DESIGN.md D10/D11):
//
//	fleet-server -config fleet.yml
//	fleet-server -config fleet.yml -bootstrap my-fleet   # first run: prints an enrollment key
//	FLEET_BOOTSTRAP_FLEET=my-fleet FLEET_BOOTSTRAP_ENROLL_KEY=... fleet-server
//	                                                     # declarative alternative: the key is
//	                                                     # chosen by the operator (e.g. a CI secret)
//	fleet-server -version                                # stamped at build time
//	fleet-server -healthcheck                            # container HEALTHCHECK: GET /healthz
//
// Every config key can also come from FLEET_* environment variables (see
// internal/config), which is how the container image is configured.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fleetplatform/server/internal/admin"
	"fleetplatform/server/internal/app"
	"fleetplatform/server/internal/config"
	"fleetplatform/server/internal/gateway"
	"fleetplatform/server/internal/store"
	"fleetplatform/server/internal/web"
)

// version is overridden at build time: -ldflags "-X main.version=0.1.0".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to bootstrap config yml (FLEET_* env vars override it)")
	bootstrap := flag.String("bootstrap", "", "create fleet by name if missing and print a fresh enrollment key")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthcheck := flag.Bool("healthcheck", false, "GET /healthz on the configured listen port and exit 0 if ok")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal(err)
	}

	if *healthcheck {
		os.Exit(runHealthcheck(cfg.Listen))
	}

	st, err := store.OpenSqlite(cfg.DB)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	if cfg.Bootstrap() {
		if err := app.Bootstrap(st, cfg.BootstrapFleet, cfg.BootstrapEnrollKey); err != nil {
			fatal(err)
		}
	}

	if *bootstrap != "" {
		fleet, ok, err := st.FleetByName(*bootstrap)
		if err != nil {
			fatal(err)
		}
		if !ok {
			if fleet, err = st.CreateFleet(*bootstrap); err != nil {
				fatal(err)
			}
			slog.Info("bootstrap: created fleet", "fleet", fleet.Name, "id", fleet.ID)
		}
		key, err := st.CreateEnrollKey(fleet.ID)
		if err != nil {
			fatal(err)
		}
		// The plaintext key is shown exactly once; only its hash is stored.
		fmt.Printf("enrollment key for fleet %q: %s\n", fleet.Name, key)
	}

	a := app.New(app.Config{
		HeartbeatInterval: cfg.HeartbeatInterval(),
		LeaseTTL:          cfg.LeaseTTL(),
		SweepEvery:        cfg.SweepEvery(),
	}, st)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.Run(ctx)

	if cfg.AdminEnabled() {
		slog.Info("admin API enabled", "path", "/api/admin/")
	}
	gw := a.Gateway()
	gw.RateLimit = gateway.RateLimit{PerSec: float64(cfg.ClientMsgsPerSec), Burst: cfg.ClientMsgsBurst}
	srv := &http.Server{Addr: cfg.Listen, Handler: web.Handler(gw, admin.Handler(st, cfg.AdminToken))}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	slog.Info("fleet-server listening", "version", version, "addr", cfg.Listen, "db", cfg.DB)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
}

// runHealthcheck probes the local server. A wildcard listen (":8080",
// "0.0.0.0:8080") is probed on loopback.
func runHealthcheck(listen string) int {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: bad listen address:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.Status)
		return 1
	}
	return 0
}

func fatal(err error) {
	slog.Error("fleet-server", "err", err)
	os.Exit(1)
}
