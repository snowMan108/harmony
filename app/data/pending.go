package data

import (
	"context"
	"log"
	"runtime"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gammazero/workerpool"
	"github.com/go-redis/redis/v8"
	"github.com/itzmeanjan/harmony/app/config"
	"github.com/itzmeanjan/harmony/app/listen"
)

// PendingPool - Currently present pending tx(s) i.e. which are ready to
// be mined in next block
type PendingPool struct {
	Transactions      map[common.Hash]*MemPoolTx
	TxsFromAddress    map[common.Address]TxList
	DroppedTxs        map[common.Hash]bool
	AscTxsByGasPrice  TxList
	DescTxsByGasPrice TxList
	Lock              *sync.RWMutex
	AddTxChan         chan AddRequest
	RemoveTxChan      chan RemoveRequest
	TxExistsChan      chan ExistsRequest
	GetTxChan         chan GetRequest
	CountTxsChan      chan CountRequest
	ListTxsChan       chan ListRequest
	TxsFromAChan      chan TxsFromARequest
	PubSub            *redis.Client
	RPC               *rpc.Client
}

// HasBeenAllocatedFor - Given an address from which tx was sent
// is being checked for whether we've allocated some memory space for keeping txs
// in pool, only sent from that specific address or not
//
// @note This function is supposed to be invoked when lock is already held
func (p *PendingPool) hasBeenAllocatedFor(addr common.Address) bool {

	_, ok := p.TxsFromAddress[addr]
	return ok

}

// AllocateFor - If memory for keeping txs sent from some
// specifiic address is not allocated yet, it'll do so
// for 16 tx entries
//
// It means, it can at max keep 16 txs from same address in pool, primarily
// But it can be incremented if required
//
// @note This function is supposed to be invoked when lock is already held
func (p *PendingPool) allocateFor(addr common.Address) TxList {

	if p.hasBeenAllocatedFor(addr) {
		return p.TxsFromAddress[addr]
	}

	p.TxsFromAddress[addr] = make(TxsFromAddressAsc, 0, 16)
	return p.TxsFromAddress[addr]

}

// Start - This method is supposed to be run as an independent
// go routine, maintaining pending pool state, through out its life time
func (p *PendingPool) Start(ctx context.Context) {

	// Closure for checking whether adding new tx triggers
	// condition for dropping some other tx
	//
	// Selecting which tx to be dropped
	//
	// - Tx with lowest gas price paid ✅
	// - Oldest tx living in mempool ❌
	// - Oldest tx with lowest gas price paid ❌
	//
	// ✅ : Implemented
	// ❌ : Not yet
	//
	// @note Don't accept tx which are already dropped
	needToDropTxs := func() bool {

		p.Lock.RLock()
		defer p.Lock.RUnlock()

		return uint64(p.AscTxsByGasPrice.len())+1 > config.GetPendingPoolSize()

	}

	pickTxWithLowestGasPrice := func() *MemPoolTx {

		p.Lock.RLock()
		defer p.Lock.RUnlock()

		return p.AscTxsByGasPrice.get()[0]

	}

	// Plain simple safe tx adding into pool, logic, invoke it from other section
	//
	// Don't rewrite this logic again
	//
	// @note Just make sure, you don't hold lock, when you call
	// this
	addTx := func(tx *MemPoolTx) {

		// -- Safe writing, critical section begins
		p.Lock.Lock()
		defer p.Lock.Unlock()

		p.AscTxsByGasPrice = Insert(p.AscTxsByGasPrice, tx)
		p.DescTxsByGasPrice = Insert(p.DescTxsByGasPrice, tx)
		p.TxsFromAddress[tx.From] = Insert(p.allocateFor(tx.From), tx)
		p.Transactions[tx.Hash] = tx

	}

	// Plain simple remove tx logic, use it everywhere else
	//
	// @note Just make sure, you don't hold lock, when you call
	// this
	removeTx := func(tx *MemPoolTx) {

		// -- Safe writing, critical section begins
		p.Lock.Lock()
		defer p.Lock.Unlock()

		// Remove from sorted tx list, keep it sorted
		p.AscTxsByGasPrice = Remove(p.AscTxsByGasPrice, tx)
		p.DescTxsByGasPrice = Remove(p.DescTxsByGasPrice, tx)
		p.TxsFromAddress[tx.From] = Remove(p.TxsFromAddress[tx.From], tx)
		delete(p.Transactions, tx.Hash)

	}

	// Silently drop some tx, before adding
	// new one, so that we don't exceed limit
	// set up by user
	dropTx := func(tx *MemPoolTx) {

		removeTx(tx)
		// 👇 op not being done while holding lock
		// is due to the fact, no other competing
		// worker attempting to read from/ write to
		// this one, now
		p.DroppedTxs[tx.Hash] = true

	}

	// Closure for safely adding new tx into pool
	txAdder := func(tx *MemPoolTx) bool {

		// -- Safe reading begins
		p.Lock.RLock()

		if _, ok := p.Transactions[tx.Hash]; ok {
			p.Lock.RUnlock()
			return false
		}

		if _, ok := p.DroppedTxs[tx.Hash]; ok {
			p.Lock.RUnlock()
			return false
		}

		p.Lock.RUnlock()
		// -- reading ends

		if needToDropTxs() {
			dropTx(pickTxWithLowestGasPrice())
			log.Printf("🧹 Dropped pending tx, was about to hit limit\n")
		}

		// Marking we found this tx in mempool now
		tx.PendingFrom = time.Now().UTC()
		tx.Pool = "pending"

		// Don't hold lock here, it'll be attempted to be acquired
		// in called function
		addTx(tx)

		// After adding new tx in pending pool, also attempt to
		// publish it to pubsub topic
		p.PublishAdded(ctx, p.PubSub, tx)

		return true

	}

	// Just a closure, which will remove existing tx
	// from pending pool, assuming it has been confirmed/ dropped
	//
	// This is extracted out here, for ease of usability
	txRemover := func(txStat *TxStatus) bool {

		// -- Safe reading, begins
		p.Lock.RLock()

		tx, ok := p.Transactions[txStat.Hash]
		if !ok {

			p.Lock.RUnlock()
			return false

		}

		p.Lock.RUnlock()
		// -- reading ends

		// Tx got confirmed/ dropped, to be used when computing
		// how long it spent in pending pool
		if txStat.Status == DROPPED {
			tx.Pool = "dropped"
			tx.DroppedAt = time.Now().UTC()
		}

		if txStat.Status == CONFIRMED {
			tx.Pool = "confirmed"
			tx.ConfirmedAt = time.Now().UTC()
		}

		// Don't hold lock here, it'll be attempted to be acquired
		// in called function
		removeTx(tx)

		// Publishing this confirmed/ dropped tx
		p.PublishRemoved(ctx, p.PubSub, tx)

		return true

	}

	for {

		select {

		case <-ctx.Done():
			return

		case req := <-p.AddTxChan:

			req.ResponseChan <- txAdder(req.Tx)

		case req := <-p.RemoveTxChan:

			req.ResponseChan <- txRemover(req.TxStat)

		case req := <-p.TxExistsChan:

			// -- Safe reading begins
			p.Lock.RLock()

			_, ok := p.Transactions[req.Tx]

			p.Lock.RUnlock()
			// -- ends

			req.ResponseChan <- ok

		case req := <-p.GetTxChan:

			// -- Safe reading begins
			p.Lock.RLock()

			if tx, ok := p.Transactions[req.Tx]; ok {
				req.ResponseChan <- tx

				p.Lock.RUnlock()
				break
			}

			p.Lock.RUnlock()
			// -- ends

			req.ResponseChan <- nil

		case req := <-p.CountTxsChan:

			// -- Safe reading begins
			p.Lock.RLock()

			req.ResponseChan <- uint64(p.AscTxsByGasPrice.len())

			p.Lock.RUnlock()
			// -- ends

		case req := <-p.ListTxsChan:

			if req.Order == ASC {
				// -- Safe reading begins
				p.Lock.RLock()

				// If empty, just return nil
				if p.AscTxsByGasPrice.len() == 0 {
					req.ResponseChan <- nil

					p.Lock.RUnlock()
					break
				}

				copied := make([]*MemPoolTx, p.AscTxsByGasPrice.len())
				copy(copied, p.AscTxsByGasPrice.get())

				req.ResponseChan <- copied

				p.Lock.RUnlock()
				// -- ends
				break
			}

			if req.Order == DESC {
				// -- Safe reading begins
				p.Lock.RLock()

				// If empty, just return nil
				if p.DescTxsByGasPrice.len() == 0 {
					req.ResponseChan <- nil

					p.Lock.RUnlock()
					break
				}

				copied := make([]*MemPoolTx, p.DescTxsByGasPrice.len())
				copy(copied, p.DescTxsByGasPrice.get())

				req.ResponseChan <- copied

				p.Lock.RUnlock()
				// -- ends
			}

		case req := <-p.TxsFromAChan:
			// Return only those txs, which were sent by specific address `A`

			if txs, ok := p.TxsFromAddress[req.From]; ok {

				if txs.len() == 0 {
					req.ResponseChan <- nil
					break
				}

				copied := make([]*MemPoolTx, txs.len())
				copy(copied, txs.get())

				req.ResponseChan <- copied
				break

			}

			req.ResponseChan <- nil

		}

	}

}

// Prune - Remove confirmed/ dropped txs from pending pool
//
// @note This method is supposed to be run as independent go routine
func (p *PendingPool) Prune(ctx context.Context, commChan chan listen.CaughtTxs) {

	// Creating worker pool, where jobs to be submitted
	// for concurrently checking whether tx was dropped or not
	wp := workerpool.New(config.GetConcurrencyFactor())
	defer wp.Stop()

	internalChan := make(chan *TxStatus, 1024)
	var droppedOrConfirmed uint64

	for {

		select {

		case <-ctx.Done():
			return

		case txs := <-commChan:

			var prunables []*MemPoolTx = make([]*MemPoolTx, 0, len(txs))
			var alreadyAddedFromA map[common.Address]hexutil.Uint64 = make(map[common.Address]hexutil.Uint64)

			for i := 0; i < len(txs); i++ {

				tx := p.Get(txs[i].Hash)
				if tx == nil {
					// well, couldn't find tx in pool
					continue
				}

				if workedOnNonce, ok := alreadyAddedFromA[tx.From]; ok && workedOnNonce > tx.Nonce {
					// well, we've already added tx into `prunables`, because we're already
					// done with processing some tx with higher nonce & this tx was included there
					continue
				}

				prunables = append(prunables, p.Prunables(tx)...)
				alreadyAddedFromA[tx.From] = tx.Nonce

			}

			for i := 0; i < len(prunables); i++ {

				func(tx *MemPoolTx) {

					wp.Submit(func() {

						// Tx got confirmed/ dropped, to be used when computing
						// how long it spent in pending pool
						dropped, _ := tx.IsDropped(ctx, p.RPC)
						if dropped {

							internalChan <- &TxStatus{Hash: tx.Hash, Status: DROPPED}
							return

						}

						internalChan <- &TxStatus{Hash: tx.Hash, Status: CONFIRMED}

					})

				}(prunables[i])

			}

		case tx := <-internalChan:

			if tx.Status == CONFIRMED || tx.Status == DROPPED {

				// Keep pruning as soon as we determined it can be pruned, rather than wait
				// for all to come & then doing it
				if p.Remove(ctx, tx) {
					droppedOrConfirmed++

					if droppedOrConfirmed%10 == 0 {
						log.Printf("[➖] Removed 10 tx(s) from pending tx pool\n")
					}
				}

			}

		}

	}

}

// Get - Given tx hash, attempts to find out tx in pending pool, if any
//
// Returns nil, if found nothing
func (p *PendingPool) Get(hash common.Hash) *MemPoolTx {

	respChan := make(chan *MemPoolTx)

	p.GetTxChan <- GetRequest{Tx: hash, ResponseChan: respChan}

	return <-respChan

}

// Exists - Checks whether tx of given hash exists on pending pool or not
func (p *PendingPool) Exists(hash common.Hash) bool {

	respChan := make(chan bool)

	p.TxExistsChan <- ExistsRequest{Tx: hash, ResponseChan: respChan}

	return <-respChan

}

// Count - How many tx(s) currently present in pending pool
func (p *PendingPool) Count() uint64 {

	respChan := make(chan uint64)

	p.CountTxsChan <- CountRequest{ResponseChan: respChan}

	return <-respChan

}

// Prunables - Given tx, we're attempting to find out all txs which are living
// in pending pool now & having same sender address & same/ lower nonce, so that
// pruner can update state while removing mined txs from mempool
func (p *PendingPool) Prunables(targetTx *MemPoolTx) []*MemPoolTx {

	txs := p.TxsFromA(targetTx.From)
	if txs == nil {
		return nil
	}

	txCount := uint64(len(txs))
	commChan := make(chan *MemPoolTx, txCount)
	result := make([]*MemPoolTx, 0, txCount)
	// This is the tx which got mined
	result = append(result, targetTx)

	var wp *workerpool.WorkerPool

	if txCount > uint64(runtime.NumCPU()) {
		wp = workerpool.New(runtime.NumCPU())
	} else {
		wp = workerpool.New(int(txCount))
	}

	for i := 0; i < len(txs); i++ {

		func(tx *MemPoolTx) {

			wp.Submit(func() {

				if tx.Hash != targetTx.Hash && tx.Nonce <= targetTx.Nonce {
					commChan <- tx
					return
				}

				commChan <- nil

			})

		}(txs[i])

	}

	var received uint64

	// Waiting for all go routines to finish
	for v := range commChan {

		if v != nil {
			result = append(result, v)
		}

		received++
		if received >= txCount {
			break
		}

	}

	// This call is irrelevant here, probably, but still being made
	//
	// Because all workers have exited, otherwise we could have never
	// reached this point
	wp.Stop()

	return result

}

// DuplicateTxs - Attempting to find duplicate tx(s) for given
// txHash.
//
// @note In duplicate tx list, the tx which was provided as input
// will not be included
//
// Considering one tx duplicate of given one, if this tx has same
// nonce & sender address, as of given ones
func (p *PendingPool) DuplicateTxs(hash common.Hash) []*MemPoolTx {

	targetTx := p.Get(hash)
	if targetTx == nil {
		return nil
	}

	txs := p.DescListTxs()
	if txs == nil {
		return nil
	}

	txCount := uint64(len(txs))
	commChan := make(chan *MemPoolTx, txCount)
	result := make([]*MemPoolTx, 0, txCount)

	// Attempting to concurrently checking which txs are duplicate
	// of a given tx hash
	wp := workerpool.New(runtime.NumCPU())

	for i := 0; i < len(txs); i++ {

		func(tx *MemPoolTx) {

			wp.Submit(func() {

				if tx.IsDuplicateOf(targetTx) {
					commChan <- tx
					return
				}

				commChan <- nil

			})

		}(txs[i])

	}

	var received uint64
	mustReceive := txCount

	// Waiting for all go routines to finish
	for v := range commChan {

		if v != nil {
			result = append(result, v)
		}

		received++
		if received >= mustReceive {
			break
		}

	}

	// This call is irrelevant here, probably, but still being made
	//
	// Because all workers have exited, otherwise we could have never
	// reached this point
	wp.Stop()

	return result

}

// AscListTxs - Returns all tx(s) present in pending pool, as slice, ascending ordered as per gas price paid
func (p *PendingPool) AscListTxs() []*MemPoolTx {

	respChan := make(chan []*MemPoolTx)

	p.ListTxsChan <- ListRequest{ResponseChan: respChan, Order: ASC}

	return <-respChan

}

// DescListTxs - Returns all tx(s) present in pending pool, as slice, descending ordered as per gas price paid
func (p *PendingPool) DescListTxs() []*MemPoolTx {

	respChan := make(chan []*MemPoolTx)

	p.ListTxsChan <- ListRequest{ResponseChan: respChan, Order: DESC}

	return <-respChan

}

// TxsFromA - Returns a slice of txs, where all of those are sent
// by address `A`
func (p *PendingPool) TxsFromA(addr common.Address) []*MemPoolTx {

	respChan := make(chan []*MemPoolTx)

	p.TxsFromAChan <- TxsFromARequest{ResponseChan: respChan, From: addr}

	return <-respChan

}

// TopXWithHighGasPrice - Returns only top `X` tx(s) present in pending mempool,
// where being top is determined by how much gas price paid by tx sender
func (p *PendingPool) TopXWithHighGasPrice(x uint64) []*MemPoolTx {
	return p.DescListTxs()[:x]
}

// TopXWithLowGasPrice - Returns only top `X` tx(s) present in pending mempool,
// where being top is determined by how low gas price paid by tx sender
func (p *PendingPool) TopXWithLowGasPrice(x uint64) []*MemPoolTx {
	return p.AscListTxs()[:x]
}

// SentFrom - Returns a list of pending tx(s) sent from
// specified address
func (p *PendingPool) SentFrom(address common.Address) []*MemPoolTx {

	txs := p.DescListTxs()
	if txs == nil {
		return nil
	}

	txCount := uint64(len(txs))
	commChan := make(chan *MemPoolTx, txCount)
	result := make([]*MemPoolTx, 0, txCount)

	wp := workerpool.New(runtime.NumCPU())

	for i := 0; i < len(txs); i++ {

		func(tx *MemPoolTx) {

			wp.Submit(func() {

				if tx.IsSentFrom(address) {
					commChan <- tx
					return
				}

				commChan <- nil

			})

		}(txs[i])

	}

	var received uint64
	mustReceive := txCount

	// Waiting for all go routines to finish
	for v := range commChan {

		if v != nil {
			result = append(result, v)
		}

		received++
		if received >= mustReceive {
			break
		}

	}

	// This call is irrelevant here, probably, but still being made
	//
	// Because all workers have exited, otherwise we could have never
	// reached this point
	wp.Stop()

	return result

}

// SentTo - Returns a list of pending tx(s) sent to
// specified address
func (p *PendingPool) SentTo(address common.Address) []*MemPoolTx {

	txs := p.DescListTxs()
	if txs == nil {
		return nil
	}

	txCount := uint64(len(txs))
	commChan := make(chan *MemPoolTx, txCount)
	result := make([]*MemPoolTx, 0, txCount)

	wp := workerpool.New(runtime.NumCPU())

	for i := 0; i < len(txs); i++ {

		func(tx *MemPoolTx) {

			wp.Submit(func() {

				if tx.IsSentTo(address) {
					commChan <- tx
					return
				}

				commChan <- nil

			})

		}(txs[i])

	}

	var received uint64
	mustReceive := txCount

	// Waiting for all go routines to finish
	for v := range commChan {

		if v != nil {
			result = append(result, v)
		}

		received++
		if received >= mustReceive {
			break
		}

	}

	// This call is irrelevant here, probably, but still being made
	//
	// Because all workers have exited, otherwise we could have never
	// reached this point
	wp.Stop()

	return result

}

// OlderThanX - Returns a list of all pending tx(s), which are
// living in mempool for more than or equals to `X` time unit
func (p *PendingPool) OlderThanX(x time.Duration) []*MemPoolTx {

	txs := p.DescListTxs()
	if txs == nil {
		return nil
	}

	txCount := uint64(len(txs))
	commChan := make(chan *MemPoolTx, txCount)
	result := make([]*MemPoolTx, 0, txCount)

	wp := workerpool.New(runtime.NumCPU())

	for i := 0; i < len(txs); i++ {

		func(tx *MemPoolTx) {

			wp.Submit(func() {

				if tx.IsPendingForGTE(x) {
					commChan <- tx
					return
				}

				commChan <- nil

			})

		}(txs[i])

	}

	var received uint64
	mustReceive := txCount

	// Waiting for all go routines to finish
	for v := range commChan {

		if v != nil {
			result = append(result, v)
		}

		received++
		if received >= mustReceive {
			break
		}

	}

	// This call is irrelevant here, probably, but still being made
	//
	// Because all workers have exited, otherwise we could have never
	// reached this point
	wp.Stop()

	return result

}

// FresherThanX - Returns a list of all pending tx(s), which are
// living in mempool for less than or equals to `X` time unit
func (p *PendingPool) FresherThanX(x time.Duration) []*MemPoolTx {

	txs := p.DescListTxs()
	if txs == nil {
		return nil
	}

	txCount := uint64(len(txs))
	commChan := make(chan *MemPoolTx, txCount)
	result := make([]*MemPoolTx, 0, txCount)

	wp := workerpool.New(runtime.NumCPU())

	for i := 0; i < len(txs); i++ {

		func(tx *MemPoolTx) {

			wp.Submit(func() {

				if tx.IsPendingForLTE(x) {
					commChan <- tx
					return
				}

				commChan <- nil

			})

		}(txs[i])

	}

	var received uint64
	mustReceive := txCount

	// Waiting for all go routines to finish
	for v := range commChan {

		if v != nil {
			result = append(result, v)
		}

		received++
		if received >= mustReceive {
			break
		}

	}

	// This call is irrelevant here, probably, but still being made
	//
	// Because all workers have exited, otherwise we could have never
	// reached this point
	wp.Stop()

	return result

}

// Add - Attempts to add new tx found in pending pool into
// harmony mempool, so that further manipulation can be performed on it
//
// If it returns `true`, it denotes, it's success, otherwise it's failure
// because this tx is already present in pending pool
func (p *PendingPool) Add(ctx context.Context, tx *MemPoolTx) bool {

	respChan := make(chan bool)

	p.AddTxChan <- AddRequest{Tx: tx, ResponseChan: respChan}

	return <-respChan

}

// VerifiedAdd - Before adding tx from queued pool, just check do we
// really need to add this tx in pending pool i.e. is this tx really
// pending ?
func (p *PendingPool) VerifiedAdd(ctx context.Context, tx *MemPoolTx) bool {

	ok, err := tx.IsNonceExhausted(ctx, p.RPC)
	if err != nil {
		return false
	}

	if ok {
		return false
	}

	return p.Add(ctx, tx)

}

// PublishAdded - Publish new pending tx pool content ( in messagepack serialized format )
// to pubsub topic
func (p *PendingPool) PublishAdded(ctx context.Context, pubsub *redis.Client, msg *MemPoolTx) {

	_msg, err := msg.ToMessagePack()
	if err != nil {

		log.Printf("[❗️] Failed to serialize into messagepack : %s\n", err.Error())
		return

	}

	if err := pubsub.Publish(ctx, config.GetPendingTxEntryPublishTopic(), _msg).Err(); err != nil {
		log.Printf("[❗️] Failed to publish new pending tx : %s\n", err.Error())
	}

}

// Remove - Removes already existing tx from pending tx pool
// denoting it has been mined i.e. confirmed/ dropped ( possible too )
func (p *PendingPool) Remove(ctx context.Context, txStat *TxStatus) bool {

	respChan := make(chan bool)

	p.RemoveTxChan <- RemoveRequest{TxStat: txStat, ResponseChan: respChan}

	return <-respChan

}

// PublishRemoved - Publish old pending tx pool content ( in messagepack serialized format )
// to pubsub topic
//
// These tx(s) are leaving pending pool i.e. they're confirmed now
func (p *PendingPool) PublishRemoved(ctx context.Context, pubsub *redis.Client, msg *MemPoolTx) {

	_msg, err := msg.ToMessagePack()
	if err != nil {

		log.Printf("[❗️] Failed to serialize into messagepack : %s\n", err.Error())
		return

	}

	if err := pubsub.Publish(ctx, config.GetPendingTxExitPublishTopic(), _msg).Err(); err != nil {
		log.Printf("[❗️] Failed to publish confirmed tx : %s\n", err.Error())
	}

}

// AddPendings - Update latest pending pool state
func (p *PendingPool) AddPendings(ctx context.Context, txs map[string]map[string]*MemPoolTx) uint64 {

	var count uint64

	for keyO := range txs {
		for keyI := range txs[keyO] {

			if p.Add(ctx, txs[keyO][keyI]) {
				count++
			}

		}
	}

	return count

}
