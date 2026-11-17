// Command forge is the Go core of the forge: a statically linked binary the
// Node entrypoint spawns. It binds an ephemeral port, prints PORT=<n> on
// stdout, then serves the forge over HTTP until its stdin closes (the
// entrypoint keeps stdin open; when the entrypoint dies, stdin closes and
// this process exits so no orphan keeps the port).
//
// This stub serves only /healthz; the full forge (git smart HTTP, REST v3,
// GraphQL, MCP) lands here incrementally.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
)

func main() {
	// Bind the dual-stack wildcard: the env dials the entry at the address
	// the register request came from, which is ::1 when the entrypoint's
	// fetch reaches it over IPv6 loopback. An IPv4-only bind would refuse
	// those connections and fail boot.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// fmt.Printf writes straight to the fd (no user-space buffering), so
	// the entrypoint sees the line at once. Do NOT Sync stdout: syncing a
	// pipe is an error and would kill the process here.
	fmt.Printf("PORT=%d\n", port)

	// Dead-man's switch: exit when the entrypoint closes our stdin.
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := os.Stdin.Read(buf); err != nil {
				os.Exit(0)
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte("{\"ok\":true}\n"))
	})

	srv := &http.Server{Handler: mux}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
		os.Exit(1)
	}
}
