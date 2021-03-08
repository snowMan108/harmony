package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/itzmeanjan/harmony/app/bootup"
	"github.com/itzmeanjan/harmony/app/mempool"
	"github.com/itzmeanjan/harmony/app/server"
)

func main() {

	log.Printf("[😌] Harmony - Reducing Chaos in MemPool\n")

	abs, err := filepath.Abs(".env")
	if err != nil {

		log.Printf("[❗️] Failed to find absolute path of file : %s\n", err.Error())
		os.Exit(1)

	}

	// This is application's root level context, to be passed down
	// to worker go routines
	ctx, cancel := context.WithCancel(context.TODO())

	resources, err := bootup.SetGround(ctx, abs)
	if err != nil {

		log.Printf("[❗️] Failed to acquire resource(s) : %s\n", err.Error())
		os.Exit(1)

	}

	// Attempt to catch interrupt event(s)
	// so that graceful shutdown can be performed
	interruptChan := make(chan os.Signal, 1)
	comm := make(chan struct{}, 1)
	signal.Notify(interruptChan, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL)

	go func() {

		// To be invoked when returning from this
		// go rountine's execution scope
		defer func() {

			resources.Release()

			// Stopping process
			log.Printf("\n[✅] Gracefully shut down `harmony` after %s\n", time.Now().UTC().Sub(resources.StartedAt))
			os.Exit(0)

		}()

	OUTER:
		for {

			select {

			case <-interruptChan:

				// When interrupt is received, attempting to
				// let all other go routines know, master go routine
				// wants all to shut down, they must do a graceful shut down
				// of what they're doing now
				cancel()

				// Giving workers 3 seconds, before forcing shutdown
				//
				// This is simply a blocking call i.e. blocks for 3 seconds
				<-time.After(time.Second * time.Duration(3))
				break OUTER

			case <-comm:
				// Supervisor go routine got to learn
				// that go routine it spawned some time ago
				// for polling ethereum mempool content periodically
				// has died
				//
				// It's supposed to spawn new go routine for handling that op
				//
				// @note To be implemented
				break OUTER

			}

		}

	}()

	// Starting tx pool monitor as a seperate worker
	go mempool.PollTxPoolContent(ctx, resources, comm)

	// Main go routine, starts one http server &
	// interfaces with external world
	server.Start(ctx, resources)

}
