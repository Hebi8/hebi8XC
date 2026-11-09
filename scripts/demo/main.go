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
	"path/filepath"
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
	fmt.Printf("  %-7s this machine, %d sessions; code blocks, long lines and calls with files are in %q\n", d.Names[0], d.Sessions[0], d.Code)
	fmt.Printf("  %-7s online, %d sessions; the long one is %q, and %q reads a file %s changed\n", d.Names[1], d.Sessions[1], d.Long, d.Review, d.Names[0])
	fmt.Printf("  %-7s offline, %d sessions, text only; a copy is kept of %q, the others, such as %q, cannot be read\n", d.Names[2], d.Sessions[2], d.Kept, d.Unread)
	fmt.Printf("\nSearch for %q (all three machines), %q (more than one answer holds), %q (one call, deep in a long session),\n", d.Term, d.Broad, d.Anchor)
	fmt.Printf("or the file %s (changed on %s; read on %s too with the operation \"any\").\n", d.File, d.Names[0], d.Names[1])
	fmt.Printf("\nCalls: %s --data-dir %s show desk/%s\n", filepath.Join(n.Dir, "h8c"), filepath.Join(n.Dir, "node-0"), d.Code)
	fmt.Printf("\nCtrl-C stops the nodes and removes %s\n", n.Dir)

	select {
	case <-ctx.Done():
	case err := <-n.Failed():
		if ctx.Err() == nil { // not a node that Ctrl-C reached before us
			fmt.Fprintln(os.Stderr, "demo:", err)
		}
	}
}
