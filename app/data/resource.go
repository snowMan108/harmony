package data

import "github.com/ethereum/go-ethereum/rpc"

// Resource - Shared resources among multiple go routines
//
// Needs to be released carefully when shutting down
type Resource struct {
	RPCClient *rpc.Client
	Pool      *MemPool
}

// Release - To be called when application will receive shut down request
// from system, to gracefully deallocate all resources
func (r *Resource) Release() {

	r.RPCClient.Close()

}
