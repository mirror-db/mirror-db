// Package main — mirror-db relay server.
//
// Fetches the mirror manifest from the upstream Worker's /api/mirrors endpoint,
// then proxies requests by replacing the relay domain with the upstream domain.
// Designed to run behind a domestic reverse proxy.
package main

import (
	"log"
	"os/signal"
	"syscall"

	"github.com/mirror-db/mirror-db/tools/relay/config"
	"github.com/mirror-db/mirror-db/tools/relay/forward"
	"github.com/mirror-db/mirror-db/tools/relay/install"
	"github.com/mirror-db/mirror-db/tools/relay/server"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "relay",
		Short: "mirror-db relay server",
	}

	root.AddCommand(serveCmd())
	root.AddCommand(install.Cmd())
	root.AddCommand(forward.Cmd())

	if err := root.Execute(); err != nil {
		log.Fatal(err)
	}
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the relay server",
		RunE: func(cmd *cobra.Command, args []string) error {
			srv, err := server.NewServer()
			if err != nil {
				return err
			}

			// Stop the edge-selection engine on the way out; otherwise its
			// probe goroutines keep running against a network the process is no
			// longer serving.
			defer srv.Close()

			log.Printf("relay → %s", config.GetUpstreamURL())

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			served := make(chan error, 1)
			go func() { served <- server.ListenAndServeHTTPS(srv) }()

			select {
			case err := <-served:
				return err
			case <-ctx.Done():
				log.Println("shutting down")
				return nil
			}
		},
	}
}
