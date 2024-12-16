package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/savid/bandwidth/pkg/coordinator"
	"github.com/savid/bandwidth/pkg/peer"
	"github.com/sirupsen/logrus"
)

func main() {
	logLevel := flag.String("log_level", "info", "Log level (debug, info, warn, error)")
	peerAddr := flag.String("peer", "", "Remote peer to connect to")
	numConnections := flag.Int("connections", 1, "Number of concurrent connections")
	transactionThreshold := flag.Int("threshold", 3, "Minimum number of transactions to request")
	minBytesForRecord := flag.Int("min_bytes", 200000, "Minimum bytes a test must have before recording the bandwidth")
	minElapsedForRecord := flag.Duration("min_elapsed", 1*time.Millisecond, "Minimum elapsed time a test must have before recording the bandwidth")
	flag.Parse()

	if *peerAddr == "" {
		logrus.Fatal("--peer flag is required")
	}

	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		logrus.Fatal("Invalid log level")
	}
	logrus.SetLevel(level)

	svc := coordinator.New(*numConnections, *transactionThreshold, *minBytesForRecord, *minElapsedForRecord)

	// Start connections
	for i := 0; i < *numConnections; i++ {
		client := peer.New(*peerAddr, i+1, svc.TransactionChan())
		svc.AddClient(client)
		go client.Start()
	}

	// Start coordinator
	go svc.Start()

	// Wait for interrupt
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	svc.Stop()
}
