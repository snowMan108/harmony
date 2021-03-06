package networking

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log"
	"time"

	"github.com/itzmeanjan/harmony/app/config"
	"github.com/itzmeanjan/harmony/app/graph"
	"github.com/itzmeanjan/pub0sub/ops"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/multiformats/go-multiaddr"
)

// ReadFrom - Read from stream & attempt to deserialize length prefixed
// tx data received from peer, which will be acted upon
func ReadFrom(ctx context.Context, healthChan chan struct{}, rw *bufio.ReadWriter, peerId string, remote multiaddr.Multiaddr) {
	defer func() {
		close(healthChan)
	}()

OUT:
	for {
		select {
		case <-ctx.Done():
			break OUT

		default:
			buf := make([]byte, 4)

			if _, err := io.ReadFull(rw.Reader, buf); err != nil {
				if err == io.EOF {
					break
				}

				log.Printf("[❗️] Failed to read size of next chunk : %s | %s\n", err.Error(), remote)
				break
			}

			size := binary.LittleEndian.Uint32(buf)
			chunk := make([]byte, size)

			if _, err := io.ReadFull(rw.Reader, chunk); err != nil {
				if err == io.EOF {
					break
				}

				log.Printf("[❗️] Failed to read chunk from peer : %s | %s\n", err.Error(), remote)
				break
			}

			tx := graph.UnmarshalPubSubMessage(chunk)
			if tx == nil {
				log.Printf("[❗️] Failed to deserialise message from peer | %s\n", remote)
				continue
			}

			// Keeping entry of from which peer we received this tx
			// so that we don't end up sending them again same tx
			// when it'll be published on Pub/Sub topic
			tx.ReceivedFrom = peerId

			if memPool.HandleTxFromPeer(ctx, tx) {
				log.Printf("✅ New tx from peer : %d bytes | %s\n", len(chunk), remote)
				continue
			}

			log.Printf("👍 Seen tx from peer : %d bytes | %s\n", len(chunk), remote)

		}
	}
}

// WriteTo - Write to mempool changes into stream i.e. connection
// with some remote peer
func WriteTo(ctx context.Context, healthChan chan struct{}, rw *bufio.ReadWriter, peerId string, remote multiaddr.Multiaddr) {
	defer func() {
		close(healthChan)
	}()

	subscriber, err := graph.SubscribeToMemPool(ctx)
	if err != nil {
		log.Printf("[❗️] Failed to subscribe to mempool changes : %s\n", err.Error())
		return
	}

	defer func() {
		if _, err := subscriber.UnsubscribeAll(); err != nil {
			log.Printf("[❗️] Failed to unsubscribe : %s\n", err.Error())
		}
		if err := subscriber.Disconnect(); err != nil {
			log.Printf("[❗️] Failed to destroy subscriber : %s\n", err.Error())
		}
	}()

	process := func(msg *ops.PushedMessage) error {
		unmarshalled := graph.UnmarshalPubSubMessage(msg.Data)
		if unmarshalled == nil {
			return nil
		}

		// Received from same peer, no need to let them
		// know again
		if unmarshalled.ReceivedFrom == peerId {
			return nil
		}

		chunk := make([]byte, 4+len(msg.Data))
		binary.LittleEndian.PutUint32(chunk[:4], uint32(len(msg.Data)))
		n := copy(chunk[4:], msg.Data)

		if n != len(msg.Data) {
			return nil
		}

		if _, err := rw.Write(chunk); err != nil {
			return err
		}

		if err := rw.Flush(); err != nil {
			return err
		}

		return nil
	}
	duration := time.Duration(256) * time.Millisecond

OUT:
	for {
		select {
		case <-ctx.Done():
			break OUT

		case <-subscriber.Watch():
			// Listening for message availablity signal
			received := subscriber.Next()
			if received == nil {
				break
			}

			if err := process(received); err != nil {
				log.Printf("[❗️] Failed to notify peer : %s\n", err.Error())
				break OUT
			}

		case <-time.After(duration):
			// Explicitly checking for message availability in queue
			if !subscriber.Queued() {
				break
			}

			started := time.Now()
			for received := subscriber.Next(); received != nil; {
				if err := process(received); err != nil {
					log.Printf("[❗️] Failed to notify peer : %s\n", err.Error())
					break OUT
				}

				if time.Since(started) > duration {
					break
				}
			}

		}
	}
}

// HandleStream - Attepts new stream & handles it through out its life time
func HandleStream(stream network.Stream) {

	remote := stream.Conn().RemoteMultiaddr()
	peerId := stream.Conn().RemotePeer()

	// We're already connected with this peer, we're closing this stream
	if connectionManager.IsConnected(peerId) {

		log.Printf("[🙃] Duplicate connection to peer : %s, dropping\n", remote)

		// Closing stream, may be it's already closed
		if err := stream.Close(); err != nil {
			log.Printf("[❗️] Failed to close stream : %s\n", err.Error())
		}

		// Marking that we're not connected to this peer
		// We can attempt to connect to it, in future iteration
		connectionManager.Dropped(peerId)
		return

	}

	// Marking we're already connect to this peer, so
	// when next time we start discovering peers, we don't
	// connect to them again
	connectionManager.Added(peerId)

	ctx, cancel := context.WithCancel(parentCtx)
	readerHealth := make(chan struct{})
	writerHealth := make(chan struct{})
	rw := bufio.NewReadWriter(bufio.NewReader(stream), bufio.NewWriter(stream))

	go ReadFrom(ctx, readerHealth, rw, peerId.String(), remote)
	go WriteTo(ctx, writerHealth, rw, peerId.String(), remote)

	log.Printf("🤩 Got new stream from peer : %s\n", remote)

	// @note This is a blocking call
	select {
	case <-readerHealth:
	case <-writerHealth:
	}
	cancel()

	// Closing stream, may be it's already closed
	if err := stream.Close(); err != nil {
		log.Printf("[❗️] Failed to close stream : %s\n", err.Error())
	}

	// Connection manager also knows this peer can be attempted to be
	// reconnected, if founded via discovery service
	connectionManager.Dropped(peerId)
	log.Printf("🙂 Dropped peer connection : %s\n", remote)

}

// Listen - Handle incoming connection of other harmony peer for certain supported
// protocol(s)
func Listen(_host host.Host) {
	_host.SetStreamHandler(protocol.ID(config.GetNetworkingStream()), HandleStream)
}
