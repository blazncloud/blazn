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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWith(ctx, os.Getenv, runtimeFactories{
		newBackend: func(config sandboxcontroller.KubernetesConfig) (sandboxcontroller.Backend, error) {
			return sandboxcontroller.NewKubernetesBackendFromConfig(config)
		},
		openStore: openPostgresStore,
		newController: func(store sandboxcontroller.Store, backend sandboxcontroller.Backend, config sandboxcontroller.Config) (controllerRunner, error) {
			return sandboxcontroller.New(store, backend, config)
		},
	})
}

type controllerRunner interface{ Run(context.Context) error }

type runtimeFactories struct {
	newBackend    func(sandboxcontroller.KubernetesConfig) (sandboxcontroller.Backend, error)
	openStore     func(string) (sandboxcontroller.Store, error)
	newController func(sandboxcontroller.Store, sandboxcontroller.Backend, sandboxcontroller.Config) (controllerRunner, error)
}

func runWith(ctx context.Context, getenv func(string) string, factories runtimeFactories) int {
	config, err := sandboxcontroller.ConfigFromEnv(getenv)
	if err != nil {
		log.Print("sandbox controller configuration is invalid")
		return 2
	}
	if factories.newBackend == nil || factories.openStore == nil || factories.newController == nil {
		log.Print("sandbox controller runtime factories are invalid")
		return 2
	}
	backend, err := factories.newBackend(config.Kubernetes)
	if err != nil {
		log.Print("sandbox controller Kubernetes backend initialization failed")
		return 2
	}
	store, err := factories.openStore(config.DatabaseURL)
	if err != nil {
		log.Print("sandbox controller store initialization failed")
		return 1
	}
	defer store.Close()
	controller, err := factories.newController(store, backend, config.Controller)
	if err != nil {
		log.Print("sandbox controller initialization failed")
		return 2
	}
	if err := controller.Run(ctx); err != nil {
		log.Print("sandbox controller execution failed")
		return 1
	}
	return 0
}

func openPostgresStore(databaseURL string) (sandboxcontroller.Store, error) {
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	store, err := sandboxcontroller.NewPgStore(database)
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}
