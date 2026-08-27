package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
		// Configuration, backend, controller, and run errors name only the
		// offending field, file, or operation (never a secret value), so
		// surfacing them is safe and makes a failing deployment diagnosable
		// without a shell in the scratch image.
		log.Printf("sandbox controller configuration is invalid: %v", err)
		return 2
	}
	if factories.newBackend == nil || factories.openStore == nil || factories.newController == nil {
		log.Print("sandbox controller runtime factories are invalid")
		return 2
	}
	backend, err := factories.newBackend(config.Kubernetes)
	if err != nil {
		log.Printf("sandbox controller Kubernetes backend initialization failed: %v", err)
		return 2
	}
	store, err := factories.openStore(config.DatabaseURL)
	if err != nil {
		// Deliberately generic: a driver error here can echo the DSN,
		// which carries the database password.
		log.Print("sandbox controller store initialization failed")
		return 1
	}
	defer store.Close()
	controller, err := factories.newController(store, backend, config.Controller)
	if err != nil {
		log.Printf("sandbox controller initialization failed: %v", err)
		return 2
	}
	listenAddress := getenv("BLAZN_SANDBOX_ACCESS_LISTEN")
	if listenAddress == "" {
		if err := controller.Run(ctx); err != nil {
			log.Printf("sandbox controller execution failed: %v", err)
			return 1
		}
		return 0
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		log.Print("sandbox access listener initialization failed")
		return 2
	}
	accessStore, ok := store.(sandboxcontroller.AccessGrantStore)
	if !ok {
		_ = listener.Close()
		log.Print("sandbox access store is unavailable")
		return 2
	}
	accessRuntime, err := sandboxcontroller.NewKubernetesAccessRuntime(config.Kubernetes)
	if err != nil {
		_ = listener.Close()
		log.Printf("sandbox access Kubernetes initialization failed: %v", err)
		return 2
	}
	accessHandler, err := sandboxcontroller.NewAccessHandler(accessStore, accessRuntime)
	if err != nil {
		_ = listener.Close()
		log.Printf("sandbox access initialization failed: %v", err)
		return 2
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: accessHandler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 5 * time.Second}
	controllerErrors, serverErrors := make(chan error, 1), make(chan error, 1)
	go func() { controllerErrors <- controller.Run(runCtx) }()
	go func() { serverErrors <- server.Serve(listener) }()
	var runErr error
	select {
	case runErr = <-controllerErrors:
	case runErr = <-serverErrors:
		if errors.Is(runErr, http.ErrServerClosed) {
			runErr = nil
		}
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	if runErr != nil {
		log.Printf("sandbox controller execution failed: %v", runErr)
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
