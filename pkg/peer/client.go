package peer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethpandaops/ethcore/pkg/execution/mimicry"
	"github.com/jellydator/ttlcache/v3"
	"github.com/savid/bandwidth/pkg/networks"
	"github.com/sirupsen/logrus"
)

type State string

const (
	Initializing State = "initializing"
	Connected    State = "connected"
	Disconnected State = "disconnected"
)

type ForkID struct {
	Hash string
	Next string
}

type Client struct {
	peer            string
	peerNum         int
	mu              sync.RWMutex
	state           State
	transactionChan chan<- common.Hash
	stopChan        chan struct{}

	reconnectDelay time.Duration

	mimicryClient      *mimicry.Client
	nodeRecord         string
	sharedCache        *ttlcache.Cache[string, bool]
	network            *networks.Network
	chainConfig        *params.ChainConfig
	signer             types.Signer
	name               string
	protocolVersion    uint64
	implementation     string
	capabilities       *[]p2p.Cap
	version            string
	forkID             *ForkID
	lastTxTime         time.Time
	stopped            bool
	connectionAttempts int
	log                logrus.FieldLogger
}

func New(peerAddr string, peerNum int, txChan chan<- common.Hash) *Client {
	c := &Client{
		peer:            peerAddr,
		peerNum:         peerNum,
		state:           Initializing,
		transactionChan: txChan,
		stopChan:        make(chan struct{}),
		reconnectDelay:  5 * time.Second,
		sharedCache:     ttlcache.New[string, bool](),
		network: &networks.Network{
			Name: networks.NetworkNameNone,
		},
		lastTxTime: time.Now(),
	}

	c.log = logrus.WithFields(logrus.Fields{
		"peer": c.peer,
		"num":  c.peerNum,
	})
	c.log.Debug("Client initialized")

	return c
}

func (c *Client) Start() {
	c.log.Debug("Starting connection logic")

	for {
		select {
		case <-c.stopChan:
			return
		default:
			if c.GetState() != Connected {
				// Sleep before reconnecting if disconnected
				if c.GetState() == Disconnected {
					time.Sleep(c.reconnectDelay)
				}

				err := retry.Do(
					func() error {
						return c.connect()
					},
					retry.OnRetry(func(n uint, err error) {
						c.log.WithError(err).
							WithField("attempt", n).
							Debug("Connection failed, retrying")
					}),
					retry.Delay(5*time.Second),
					retry.DelayType(retry.FixedDelay),
				)

				if err != nil {
					c.log.WithError(err).Debug("Failed to connect after retries")
				}
			} else {
				time.Sleep(time.Second) // Prevent tight loop when connected
			}
		}
	}
}

func (c *Client) connect() error {
	c.setState(Initializing)
	c.log.Debug("Initializing connection")

	ctx := context.Background()
	var err error
	c.mimicryClient, err = mimicry.New(ctx, logrus.StandardLogger(), c.peer, "gumpinator")
	if err != nil {
		c.log.WithError(err).Error("Failed to create mimicry client")
		c.setState(Disconnected)
		return err
	}

	c.setupMimicryHandlers(ctx)

	err = c.mimicryClient.Start(ctx)
	if err != nil {
		c.log.WithError(err).Error("Failed to start mimicry client")
		c.setState(Disconnected)
		return err
	}

	c.setState(Connected)
	c.log.Debug("Connection established")

	return nil
}

func (c *Client) setupMimicryHandlers(ctx context.Context) {
	c.log.Debug("Setting up mimicry client handlers")

	c.mimicryClient.OnHello(ctx, func(ctx context.Context, hello *mimicry.Hello) error {
		c.log.Debug("Received Hello message from client")
		split := strings.SplitN(hello.Name, "/", 2)
		c.implementation = strings.ToLower(split[0])

		if len(split) > 1 {
			c.version = split[1]
		}

		c.name = hello.Name
		c.capabilities = &hello.Caps
		c.protocolVersion = hello.Version

		c.log.Debugf("Connected: %s/%s", c.implementation, c.version)
		c.log.Debugf("Capabilities: %+v", *c.capabilities)
		c.setState(Connected)
		return nil
	})

	c.mimicryClient.OnStatus(ctx, func(ctx context.Context, status *mimicry.Status) error {
		c.log.Debug("Received Status message from client")
		c.network = networks.DeriveFromID(status.NetworkID)
		c.forkID = &ForkID{
			Hash: fmt.Sprintf("0x%x", status.ForkID.Hash),
			Next: fmt.Sprintf("%d", status.ForkID.Next),
		}

		switch c.network.Name {
		case networks.NetworkNameMainnet:
			c.chainConfig = params.MainnetChainConfig
		case networks.NetworkNameHolesky:
			c.chainConfig = params.HoleskyChainConfig
		case networks.NetworkNameSepolia:
			c.chainConfig = params.SepoliaChainConfig
		default:
			c.chainConfig = params.MainnetChainConfig
		}

		chainID := new(big.Int).SetUint64(c.network.ID)
		c.signer = types.NewCancunSigner(chainID)

		c.log.Debugf("Status Update: network=%s, fork_id=%s, chain_id=%s", c.network.Name, c.forkID.Hash, chainID.String())
		return nil
	})

	c.mimicryClient.OnNewPooledTransactionHashes(ctx, func(ctx context.Context, hashes *mimicry.NewPooledTransactionHashes) error {
		c.mu.Lock()
		defer c.mu.Unlock()

		c.lastTxTime = time.Now()

		for index, t := range hashes.Types {
			if t == 0x03 {
				hash := hashes.Hashes[index]
				c.transactionChan <- hash
			}
		}
		return nil
	})

	c.mimicryClient.OnTransactions(ctx, func(ctx context.Context, txs *mimicry.Transactions) error {
		c.log.Debugf("Received Transactions from client: %d transactions", len(*txs))
		c.mu.Lock()
		c.lastTxTime = time.Now()
		c.mu.Unlock()
		c.log.Debugf("Updated lastTxTime to %s after processing transactions", c.lastTxTime.String())
		return nil
	})

	c.mimicryClient.OnDisconnect(ctx, func(ctx context.Context, reason *mimicry.Disconnect) error {
		str := "unknown"
		if reason != nil {
			str = reason.Reason.String()
		}

		c.log.Infof("Disconnected: reason=%s", str)
		c.log.Debug("Setting state to Disconnected")
		c.setState(Disconnected)

		c.log.Debug("Stopping mimicry client due to disconnection")
		err := c.mimicryClient.Stop(ctx)
		if err != nil {
			c.log.WithError(err).Error("Error stopping client on disconnect")
		} else {
			c.log.Debug("Client successfully stopped after disconnection")
		}

		return errors.New("peer disconnected")
	})
}

func (c *Client) RequestTransactions(txs []common.Hash) (*mimicry.WrappedPooledTransactions, error) {
	c.log.WithField("tx_count", len(txs)).Debug("Requesting pooled transactions")
	ctx := context.Background()

	wrapped, err := c.mimicryClient.GetPooledTransactions(ctx, txs)
	if err != nil {
		c.log.WithError(err).Error("Error getting pooled transactions")
		return nil, err
	}

	c.log.Debug("Successfully fetched pooled transactions")
	return wrapped, nil
}

func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		c.log.Debug("Stop called more than once, ignoring")
		return
	}

	c.log.Info("Stopping client")
	c.stopped = true
	close(c.stopChan)

	if c.mimicryClient != nil {
		c.log.Debug("Stopping mimicry client")
		err := c.mimicryClient.Stop(context.Background())
		if err != nil {
			c.log.WithError(err).Error("Error stopping mimicry client")
		} else {
			c.log.Info("Mimicry client successfully stopped")
		}
	}

	c.log.Info("Client successfully stopped")
}

// Thread-safe state getters and setters
func (c *Client) GetState() State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

func (c *Client) setState(newState State) {
	c.mu.Lock()
	c.state = newState
	c.mu.Unlock()
}
