// Command forge is the Go core of the forge: a statically linked binary the
// Node entrypoint spawns. It binds an ephemeral port, prints PORT=<n> on
// stdout, then serves the forge over HTTP until its stdin closes (the
// entrypoint keeps stdin open; when the entrypoint dies, stdin closes and
// this process exits so no orphan keeps the port).
package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Bind the dual-stack wildcard: the env dials the entry at the address
	// the register request came from, which is ::1 when the entrypoint's
	// fetch reaches it over IPv6 loopback. An IPv4-only bind would refuse
	// those connections and fail boot.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: listen: %v\n", err)
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

	// Exit promptly on SIGTERM/SIGINT.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		profDump() // [hotpath diagnostic] flush profile before exit
		os.Exit(0)
	}()

	srv := newAppHandler()
	if err := serveRaw(ln, srv); err != nil {
		fmt.Fprintf(os.Stderr, "forge: serve: %v\n", err)
		os.Exit(1)
	}
}
