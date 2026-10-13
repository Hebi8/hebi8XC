// Command demo runs three made-up machines on this one, to look at the web
// interface with: see package demo. Run it from the source tree:
//
//	go run ./scripts/demo
//
// It prints the link to open and runs until Ctrl-C, then removes what it
// wrote.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Hebi8/hebi8XC/scripts/internal/demo"
)

func main() {
	port := flag.Int("port", demo.DefaultPort, "first of the four ports to use; 0 picks free ones")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Println("starting the demo nodes…")
	n, err := demo.Start(ctx, demo.Options{Port: *port})
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
	defer n.Close()

	d := n.Data
	fmt.Printf("\n  %s\n\n", n.URL)
	fmt.Printf("  %-7s this machine, %d sessions; code blocks and long lines are in %q\n", d.Names[0], d.Sessions[0], d.Code)
	fmt.Printf("  %-7s online, %d sessions; the long one is %q\n", d.Names[1], d.Sessions[1], d.Long)
	fmt.Printf("  %-7s offline, %d sessions; a copy is kept of %q, the others cannot be read\n", d.Names[2], d.Sessions[2], d.Kept)
	fmt.Printf("\nCtrl-C stops the nodes and removes %s\n", n.Dir)

	select {
	case <-ctx.Done():
	case err := <-n.Failed():
		if ctx.Err() == nil { // not a node that Ctrl-C reached before us
			fmt.Fprintln(os.Stderr, "demo:", err)
		}
	}
}
