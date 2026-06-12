// Package main — mirror-db relay server.
//
// Fetches the mirror manifest from the upstream Worker's /@relay/ endpoint,
// then proxies requests for each mirror through /@relay/<target>/... on a
// single upstream domain. Designed to run behind a domestic reverse proxy.
package main

import "fmt"

func main() {
	fmt.Println("mirror-db relay — not yet implemented")
}
