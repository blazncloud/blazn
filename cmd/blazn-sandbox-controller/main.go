package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/blazncloud/blazn/internal/sandboxcontroller"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	os.Exit(run())
}

func run() int {
	config, err := sandboxcontroller.ConfigFromEnv(os.Getenv)
	if err != nil {
		log.Print("sandbox controller configuration is invalid")
		return 2
	}
	database, err := sql.Open("pgx", config.DatabaseURL)
	if err != nil {
		log.Print("sandbox controller database initialization failed")
		return 1
	}
	store, err := sandboxcontroller.NewPgStore(database)
	if err != nil {
		_ = database.Close()
		log.Print("sandbox controller store initialization failed")
		return 1
	}
	defer store.Close()

	// The direct controller executable is intentionally shipped with a backend
	// boundary but no cluster adapter. A real Kubernetes backend is the next,
	// separately qualified PR; fail closed rather than touching a cluster here.
	backend := sandboxcontroller.NewUnavailableBackend()
	controller, err := sandboxcontroller.New(store, backend, config.Controller)
	if err != nil {
		log.Print("sandbox controller initialization failed")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := controller.Run(ctx); err != nil {
		log.Print("sandbox controller execution failed")
		return 1
	}
	return 0
}
