// fleet-server: the control server. One process, one box (DESIGN.md D10/D11):
//
//	fleet-server -config fleet.yml
//	fleet-server -config fleet.yml -bootstrap my-fleet   # first run: prints an enrollment key
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fleetplatform/server/internal/app"
	"fleetplatform/server/internal/config"
	"fleetplatform/server/internal/store"
	"fleetplatform/server/internal/web"
)

func main() {
	configPath := flag.String("config", "", "path to bootstrap config yml")
	bootstrap := flag.String("bootstrap", "", "create fleet by name if missing and print a fresh enrollment key")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal(err)
	}

	st, err := store.OpenSqlite(cfg.DB)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

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

	srv := &http.Server{Addr: cfg.Listen, Handler: web.Handler(a.Gateway())}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	slog.Info("fleet-server listening", "addr", cfg.Listen, "db", cfg.DB)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
}

func fatal(err error) {
	slog.Error("fleet-server", "err", err)
	os.Exit(1)
}
