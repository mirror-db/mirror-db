// Package main — mirror-db relay server.
//
// Fetches the mirror manifest from the upstream Worker's /api/mirrors endpoint,
// then proxies requests by replacing the relay domain with the upstream domain.
// Designed to run behind a domestic reverse proxy.
package main

import (
	"log"

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
		Run: func(cmd *cobra.Command, args []string) {
			srv, err := server.NewServer()
			if err != nil {
				log.Fatalf("failed to create relay server: %v", err)
			}

			log.Printf("relay → %s", config.GetUpstreamURL())
			if err := server.ListenAndServeHTTPS(srv); err != nil {
				log.Fatal(err)
			}
		},
	}
}
