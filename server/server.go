package server

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func RunServer(router *http.ServeMux, configPath string) {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	var wg sync.WaitGroup

	reloadRequested := make(chan struct{}, 1)
	done := make(chan struct{}, 1)

	// signal handler
	go signalHandler(reloadRequested, done)

	// server run loop
	wg.Add(1)
	go runLoop(reloadRequested, done, &wg, router, configPath)

	wg.Wait()
	log.Info().
		Str("event", "shutdown").
		Msg("Shutdown complete")
}

func signalHandler(reloadRequested, done chan struct{}) {
	signals := make(chan os.Signal, 1)

	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)

	for {
		switch sig := <-signals; sig {

		case syscall.SIGHUP:
			reloadRequested <- struct{}{}

		case syscall.SIGINT:
			fallthrough
		case syscall.SIGTERM:
			done <- struct{}{}

		}
	}
}

func runLoop(reloadRequested, done chan struct{}, wg *sync.WaitGroup, parentRouter *http.ServeMux, configPath string) {
	var replaceableHandler *ReplaceableHandler
	if parentRouter != nil {
		replaceableHandler = &ReplaceableHandler{}
	}
	registeredPrefixes := make(map[string]struct{}, 0)

	for {
		// new server instance
		serv := UploadServer{}
		serv.cfg = *NewConfig()

		// refresh config
		err := serv.cfg.Load(configPath)
		if err != nil {
			log.Error().Err(err).Msg("Failed to load config")
		}

		// register handler on parentRouter if any, when prefix has not been previously registered
		if parentRouter != nil {
			routePrefix, err := routePrefixFromBasePath(serv.cfg.Server.BasePath)
			if err != nil {
				panic(err)
			}
			if _, ok := registeredPrefixes[routePrefix]; !ok { // this prefix not yet registered
				registeredPrefixes[routePrefix] = struct{}{}
				parentRouter.Handle(routePrefix, replaceableHandler)
				if !strings.HasSuffix(routePrefix, "/") {
					parentRouter.Handle(routePrefix+"/", replaceableHandler)
				}
				log.Info().
					Str("event", "startup").
					Str("routePrefix", routePrefix).
					Msg("Fileuploader handler mounted on parent router")
			}
		}

		errChan := make(chan error)

		// run server until .Shutdown() called or other error occurs
		go func() {
			err := serv.Run(replaceableHandler)
			if err != nil {
				errChan <- err
			}
		}()

		// wait for startup to complete
		<-serv.GetStartedChan()
		if parentRouter == nil {
			log.Info().
				Str("event", "startup").
				Str("address", serv.cfg.Server.ListenAddress).
				Msg("Server listening")
		}

		// wait for error or reload request
		shouldRestart := func() bool {
			select {

			case err := <-errChan:

				fmt.Printf("errChan: %#v\n", err)
				// quit if unexpected error occurred
				if err != http.ErrServerClosed {
					log.Fatal().
						Err(err).
						Msg("Error running upload server")
				}

				// server closed by request, exit loop to allow it to restart
				return true

			case <-reloadRequested:
				// Run in separate goroutine so we don't wait for .Shutdown()
				// to return before starting the new server.
				// This allows us to handle outstanding requests using the old
				// server instance while we've already replaced it as the listener
				// for new connections.
				go func() {
					log.Info().
						Str("event", "config_reload").
						Msg("Reloading server config")
					serv.Shutdown()
				}()
				return true

			case <-done:
				log.Info().
					Str("event", "shutdown_started").
					Msg("Shutdown initiated. Handling existing requests")
				serv.Shutdown()
				wg.Done()
				return false

			}
		}()

		if !shouldRestart {
			return
		}
	}
}
