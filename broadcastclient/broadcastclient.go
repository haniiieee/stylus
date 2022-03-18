//
// Copyright 2021-2022, Offchain Labs, Inc. All rights reserved.
//

package broadcastclient

import (
	"context"
	"encoding/json"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/pkg/errors"
	flag "github.com/spf13/pflag"

	"github.com/ethereum/go-ethereum/log"
	"github.com/offchainlabs/nitro/arbstate"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/broadcaster"
	"github.com/offchainlabs/nitro/util"
	"github.com/offchainlabs/nitro/wsbroadcastserver"
)

type FeedConfig struct {
	Output wsbroadcastserver.BroadcasterConfig `koanf:"output"`
	Input  BroadcastClientConfig               `koanf:"input"`
}

func FeedConfigAddOptions(prefix string, f *flag.FlagSet, feedInputEnable bool, feedOutputEnable bool) {
	if feedInputEnable {
		BroadcastClientConfigAddOptions(prefix+".input", f)
	}
	if feedOutputEnable {
		wsbroadcastserver.BroadcasterConfigAddOptions(prefix+".output", f)
	}
}

var FeedConfigDefault = FeedConfig{
	Output: wsbroadcastserver.DefaultBroadcasterConfig,
	Input:  DefaultBroadcastClientConfig,
}

type BroadcastClientConfig struct {
	Timeout time.Duration `koanf:"timeout"`
	URLs    []string      `koanf:"url"`
}

func (c *BroadcastClientConfig) Enable() bool {
	return len(c.URLs) > 0 && c.URLs[0] != ""
}

func BroadcastClientConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.StringSlice(prefix+".url", DefaultBroadcastClientConfig.URLs, "URL of sequencer feed source")
	f.Duration(prefix+".timeout", DefaultBroadcastClientConfig.Timeout, "duration to wait before timing out connection to sequencer feed")
}

var DefaultBroadcastClientConfig = BroadcastClientConfig{
	URLs:    []string{""},
	Timeout: 20 * time.Second,
}

type TransactionStreamerInterface interface {
	AddMessages(pos arbutil.MessageIndex, force bool, messages []arbstate.MessageWithMetadata) error
}

type BroadcastClient struct {
	util.StopWaiter

	websocketUrl    string
	lastInboxSeqNum *big.Int

	// Protects conn and shuttingDown
	connMutex sync.Mutex
	conn      net.Conn

	retryCount int64

	retrying                        bool
	shuttingDown                    bool
	ConfirmedSequenceNumberListener chan arbutil.MessageIndex
	idleTimeout                     time.Duration
	txStreamer                      TransactionStreamerInterface
}

func NewBroadcastClient(websocketUrl string, lastInboxSeqNum *big.Int, idleTimeout time.Duration, txStreamer TransactionStreamerInterface) *BroadcastClient {
	var seqNum *big.Int
	if lastInboxSeqNum == nil {
		seqNum = big.NewInt(0)
	} else {
		seqNum = lastInboxSeqNum
	}

	return &BroadcastClient{
		websocketUrl:    websocketUrl,
		lastInboxSeqNum: seqNum,
		idleTimeout:     idleTimeout,
		txStreamer:      txStreamer,
	}
}

func (bc *BroadcastClient) Start(ctxIn context.Context) {
	bc.StopWaiter.Start(ctxIn)
	bc.LaunchThread(func(ctx context.Context) {
		for {
			err := bc.connect(ctx)
			if err == nil {
				bc.startBackgroundReader()
				break
			}
			log.Warn("failed connect to sequencer broadcast, waiting and retrying", "url", bc.websocketUrl, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	})
}

func (bc *BroadcastClient) connect(ctx context.Context) error {
	if len(bc.websocketUrl) == 0 {
		// Nothing to do
		return nil
	}

	log.Info("connecting to arbitrum inbox message broadcaster", "url", bc.websocketUrl)
	timeoutDialer := ws.Dialer{
		Timeout: 10 * time.Second,
	}

	if bc.isShuttingDown() {
		return nil
	}

	conn, _, _, err := timeoutDialer.Dial(ctx, bc.websocketUrl)
	if err != nil {
		return errors.Wrap(err, "broadcast client unable to connect")
	}

	bc.connMutex.Lock()
	bc.conn = conn
	bc.connMutex.Unlock()

	log.Info("Connected")

	return nil
}

func (bc *BroadcastClient) startBackgroundReader() {
	bc.LaunchThread(func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			msg, op, err := wsbroadcastserver.ReadData(ctx, bc.conn, bc.idleTimeout, ws.StateClientSide)
			if err != nil {
				if bc.isShuttingDown() {
					return
				}
				if strings.Contains(err.Error(), "i/o timeout") {
					log.Error("Server connection timed out without receiving data", "url", bc.websocketUrl, "err", err)
				} else {
					log.Error("error calling readData", "url", bc.websocketUrl, "opcode", int(op), "err", err)
				}
				_ = bc.conn.Close()
				bc.retryConnect(ctx)
				continue
			}

			if msg != nil {
				res := broadcaster.BroadcastMessage{}
				err = json.Unmarshal(msg, &res)
				if err != nil {
					log.Error("error unmarshalling message", "msg", msg, "err", err)
					continue
				}

				if len(res.Messages) > 0 {
					log.Debug("received batch item", "count", len(res.Messages), "first seq", res.Messages[0].SequenceNumber)
				} else if res.ConfirmedSequenceNumberMessage != nil {
					log.Debug("confirmed sequence number", "seq", res.ConfirmedSequenceNumberMessage.SequenceNumber)
				} else {
					log.Debug("received broadcast with no messages populated", "length", len(msg))
				}

				if res.Version == 1 {
					if len(res.Messages) > 0 {
						messages := []arbstate.MessageWithMetadata{}
						for _, message := range res.Messages {
							messages = append(messages, message.Message)
						}
						if err := bc.txStreamer.AddMessages(res.Messages[0].SequenceNumber, false, messages); err != nil {
							log.Error("Error adding message from Sequencer Feed", "err", err)
						}
					}
					if res.ConfirmedSequenceNumberMessage != nil && bc.ConfirmedSequenceNumberListener != nil {
						bc.ConfirmedSequenceNumberListener <- res.ConfirmedSequenceNumberMessage.SequenceNumber
					}
				}
			}
		}
	})
}

func (bc *BroadcastClient) GetRetryCount() int64 {
	return atomic.LoadInt64(&bc.retryCount)
}

func (bc *BroadcastClient) isShuttingDown() bool {
	bc.connMutex.Lock()
	defer bc.connMutex.Unlock()
	return bc.shuttingDown
}

func (bc *BroadcastClient) retryConnect(ctx context.Context) {
	maxWaitDuration := 15 * time.Second
	waitDuration := 500 * time.Millisecond
	bc.retrying = true

	for !bc.isShuttingDown() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(waitDuration):
		}

		atomic.AddInt64(&bc.retryCount, 1)
		err := bc.connect(ctx)
		if err == nil {
			bc.retrying = false
			return
		}

		if waitDuration < maxWaitDuration {
			waitDuration += 500 * time.Millisecond
		}
	}
}

func (bc *BroadcastClient) StopAndWait() {
	log.Debug("closing broadcaster client connection")
	bc.StopWaiter.StopAndWait()
	bc.connMutex.Lock()
	defer bc.connMutex.Unlock()

	bc.shuttingDown = true
	if bc.conn != nil {
		_ = bc.conn.Close()
	}
}
