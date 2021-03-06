package mempool

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/itzmeanjan/harmony/app/config"
	"github.com/itzmeanjan/harmony/app/data"
)

// PollTxPoolContent - Poll current content of Ethereum Mempool periodically & do further
// processing with data received back i.e. attempt to keep most fresh view of
// mempool in `harmony`
//
// Emit events on PubSub topics for listening to state changes
func PollTxPoolContent(ctx context.Context, res *data.Resource, comm chan struct{}) {

	for {

		// Starting to fetch latest state of mempool
		start := time.Now().UTC()

		var result map[string]map[string]map[string]*data.MemPoolTx

		if err := res.RPCClient.CallContext(ctx, &result, "txpool_content"); err != nil {

			log.Printf("[❗️] Failed to fetch mempool content : %s\n", err.Error())

			// If supervisor is asking to stop operation, just get out
			// of this infinite loop
			if strings.Contains(err.Error(), "context canceled") {
				break
			}

			// Letting supervisor know, pool polling go routine is dying
			// it must take care of spawning another one to continue functioning
			close(comm)
			break

		}

		// Process current tx pool content
		res.Pool.Process(ctx, result["pending"], result["queued"])
		res.Pool.Stat(start)

		// Sleep for desired amount of time & get to work again
		<-time.After(time.Duration(config.GetMemPoolPollingPeriod()) * time.Millisecond)

	}

}
