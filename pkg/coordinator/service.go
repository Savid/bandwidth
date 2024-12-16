package coordinator

import (
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/savid/bandwidth/pkg/peer"
	"github.com/sirupsen/logrus"
)

type Service struct {
	clients              []*peer.Client
	transactions         []common.Hash
	transactionChan      chan common.Hash
	mu                   sync.RWMutex
	stopChan             chan struct{}
	transactionThreshold int

	bandwidthMu           sync.RWMutex
	bandwidthMeasurements []float64
	minBytesForRecord     int
	minElapsedForRecord   time.Duration
}

func New(numConnections, transactionThreshold, minBytesForRecord int, minElapsedForRecord time.Duration) *Service {
	return &Service{
		clients:               make([]*peer.Client, 0, numConnections),
		transactions:          make([]common.Hash, 0),
		transactionChan:       make(chan common.Hash, 1000),
		stopChan:              make(chan struct{}),
		transactionThreshold:  transactionThreshold,
		bandwidthMeasurements: make([]float64, 0, 1000),
		minBytesForRecord:     minBytesForRecord,
		minElapsedForRecord:   minElapsedForRecord,
	}
}

func (s *Service) Start() {
	go s.reportStatus()

	go s.collectTransactions()
}

func (s *Service) Stop() {
	close(s.stopChan)
	for _, client := range s.clients {
		client.Stop()
	}
}

func (s *Service) AddClient(client *peer.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients = append(s.clients, client)
}

func (s *Service) TransactionChan() chan<- common.Hash {
	return s.transactionChan
}

func (s *Service) reportStatus() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			s.mu.RLock()
			var connected, initializing, disconnected int
			for _, client := range s.clients {
				switch client.GetState() {
				case peer.Connected:
					connected++
				case peer.Initializing:
					initializing++
				case peer.Disconnected:
					disconnected++
				}
			}
			txCount := len(s.transactions)
			s.mu.RUnlock()

			s.bandwidthMu.RLock()
			measurements := make([]float64, len(s.bandwidthMeasurements))
			copy(measurements, s.bandwidthMeasurements)
			s.bandwidthMu.RUnlock()

			minBW, maxBW, p50BW, p95BW := CalculateStats(measurements)

			logrus.WithFields(logrus.Fields{
				"connected":           connected,
				"initializing":        initializing,
				"disconnected":        disconnected,
				"available_blob_txns": txCount,
				"min_bandwidth":       fmt.Sprintf("%.2f Mbit/sec", minBW/1e6),
				"max_bandwidth":       fmt.Sprintf("%.2f Mbit/sec", maxBW/1e6),
				"p50_bandwidth":       fmt.Sprintf("%.2f Mbit/sec", p50BW/1e6),
				"p95_bandwidth":       fmt.Sprintf("%.2f Mbit/sec", p95BW/1e6),
			}).Info("Status report")

			if connected == len(s.clients) {
				s.requestTransactions()
			}
		}
	}
}

func (s *Service) collectTransactions() {
	for {
		select {
		case <-s.stopChan:
			return
		case tx := <-s.transactionChan:
			s.mu.Lock()
			// Check for duplicates before appending
			isDuplicate := false
			for _, existingTx := range s.transactions {
				if existingTx == tx {
					isDuplicate = true
					break
				}
			}
			if !isDuplicate {
				s.transactions = append(s.transactions, tx)
			}
			s.mu.Unlock()
		}
	}
}

func (s *Service) requestTransactions() {
	s.mu.Lock()
	if len(s.transactions) < s.transactionThreshold {
		s.mu.Unlock()
		return
	}

	// Copy only the threshold number of transactions
	txs := make([]common.Hash, s.transactionThreshold)
	copy(txs, s.transactions[:s.transactionThreshold])

	// Remove these transactions from the front of the slice
	copy(s.transactions, s.transactions[s.transactionThreshold:])
	s.transactions = s.transactions[:len(s.transactions)-s.transactionThreshold]
	s.mu.Unlock()

	var wg sync.WaitGroup
	errors := make([]error, 0)
	var errorsMu sync.Mutex

	for num, client := range s.clients {
		wg.Add(1)
		go func(c *peer.Client, num int) {
			defer wg.Done()

			wrapped, err := c.RequestTransactions(txs)
			if err != nil {
				errorsMu.Lock()
				errors = append(errors, err)
				errorsMu.Unlock()
				return
			}

			// Calculate bandwidth if possible
			if wrapped != nil && wrapped.Elapsed >= s.minElapsedForRecord && wrapped.Bytes > s.minBytesForRecord {
				// bandwidth in bits/sec
				bandwidth := float64(wrapped.Bytes) / wrapped.Elapsed.Seconds() * 8

				s.bandwidthMu.Lock()
				s.bandwidthMeasurements = append(s.bandwidthMeasurements, bandwidth)
				// Keep only last 1000 measurements
				if len(s.bandwidthMeasurements) > 1000 {
					s.bandwidthMeasurements = s.bandwidthMeasurements[len(s.bandwidthMeasurements)-1000:]
				}
				s.bandwidthMu.Unlock()

				logrus.WithFields(logrus.Fields{
					"elapsed":   wrapped.Elapsed,
					"bytes":     wrapped.Bytes,
					"bandwidth": fmt.Sprintf("%.2f Mbit/sec", bandwidth/1e6),
					"runner":    num,
				}).Info("Bandwidth test succeeded")
			} else if wrapped != nil {
				logrus.WithFields(logrus.Fields{
					"elapsed": wrapped.Elapsed,
					"bytes":   wrapped.Bytes,
					"runner":  num,
				}).Debug("Bandwidth test not recorded (below threshold)")
			}
		}(client, num)
	}

	wg.Wait()

	if len(errors) > 0 {
		logrus.WithField("failed_clients", len(errors)).
			WithField("total_clients", len(s.clients)).
			Error("Some clients failed to process transactions")
		return
	}
}
