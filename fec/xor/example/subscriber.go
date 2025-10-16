package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"

	"github.com/mengelbart/moqtransport"
	"github.com/mengelbart/moqtransport/fec/xor"
)

// Wraps the original subscriber with FEC decoding
type XORCounterSubscriber struct {
	remoteTrack *moqtransport.RemoteTrack
	decoder     *xor.Decoder

	sessionID uint64
	namespace []string
	trackname string

	lastReceived   uint64
	totalReceived  uint64
	recoveredCount uint64
	isRunning      bool
	stopChan       chan struct{}

	mutex sync.RWMutex
}

func NewXORSubscriber(remoteTrack *moqtransport.RemoteTrack, sessionID uint64, namespace []string, trackname string) *XORCounterSubscriber {
	config := xor.Config{BlockSize: 4}
	return &XORCounterSubscriber{
		remoteTrack:    remoteTrack,
		decoder:        xor.NewDecoder(config),
		sessionID:      sessionID,
		namespace:      namespace,
		trackname:      trackname,
		lastReceived:   0,
		totalReceived:  0,
		recoveredCount: 0,
		isRunning:      false,
		stopChan:       make(chan struct{}),
	}
}

func (fcs *XORCounterSubscriber) Start(ctx context.Context) error {
	fcs.mutex.Lock()
	if fcs.isRunning {
		fcs.mutex.Unlock()
		return fmt.Errorf("XOR-FEC counter subscriber is already running")
	}
	fcs.isRunning = true
	fcs.mutex.Unlock()

	log.Printf("Starting XOR-FEC counter subscriber (session: %d, namespace: %v, track: %s)",
		fcs.sessionID, fcs.namespace, fcs.trackname)

	go fcs.receiveLoop(ctx)

	select {
	case <-ctx.Done():
		log.Printf("XOR-FEC counter subscriber stopped due to context cancellation")
		return ctx.Err()
	case <-fcs.stopChan:
		log.Printf("XOR-FEC counter subscriber stopped")
		return nil
	}
}

func (fcs *XORCounterSubscriber) Stop() {
	fcs.mutex.Lock()
	defer fcs.mutex.Unlock()

	if !fcs.isRunning {
		return
	}

	fcs.isRunning = false
	close(fcs.stopChan)
}

func (fcs *XORCounterSubscriber) receiveLoop(ctx context.Context) {
	defer func() {
		fcs.mutex.Lock()
		fcs.isRunning = false
		fcs.mutex.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-fcs.stopChan:
			return
		default:
			obj, err := fcs.remoteTrack.ReadObject(ctx)
			if err != nil {
				if err == io.EOF {
					log.Printf("Received last object - track ended")
					return
				}
				log.Printf("Error reading object: %v", err)
				return
			}

			if err := fcs.processObject(obj); err != nil {
				log.Printf("Error processing object: %v", err)
				continue
			}
		}
	}
}

func (fcs *XORCounterSubscriber) processObject(obj *moqtransport.Object) error {
	// Extract FEC metadata
	blockID, seqInBlock, isParity, payload, err := fcs.unwrapMetadata(obj.Payload)
	if err != nil {
		return fmt.Errorf("failed to unwrap XOR-FEC metadata: %w", err)
	}

	// Decode with XOR-FEC
	decodedPayload, wasRecovered := fcs.decoder.TryDecode(blockID, seqInBlock, payload, isParity)

	// Skip if it's a parity packet with no recovery
	if isParity && decodedPayload == nil {
		log.Printf("Received parity: blockID=%d", blockID)
		return nil
	}

	// Process the payload if we have one
	if decodedPayload != nil {
		counterValue, err := fcs.parseCounterValue(decodedPayload)
		if err != nil {
			return fmt.Errorf("failed to parse counter value: %w", err)
		}

		fcs.mutex.Lock()
		fcs.lastReceived = counterValue
		fcs.totalReceived++
		if wasRecovered {
			fcs.recoveredCount++
		}
		fcs.mutex.Unlock()

		recoveryStatus := ""
		if wasRecovered {
			recoveryStatus = " (RECOVERED)"
		}

		if seqInBlock == -1 {
			log.Printf("Received MoQ-Datagram: %d, blockID=%d, seqInBlock=PARITY%s",
				counterValue, blockID, recoveryStatus)
		} else {
			log.Printf("Received MoQ-Datagram: %d, blockID=%d, seqInBlock=%d%s",
				counterValue, blockID, seqInBlock, recoveryStatus)
		}

		// Cleanup old blocks periodically
		if fcs.totalReceived%10 == 0 {
			fcs.decoder.CleanupOldBlocks(blockID)
		}
	}

	return nil
}

// Extract FEC metadata from the payload
func (fcs *XORCounterSubscriber) unwrapMetadata(wrappedPayload []byte) (blockID, seqInBlock int, isParity bool, payload []byte, err error) {
	if len(wrappedPayload) < 9 {
		return 0, 0, false, nil, fmt.Errorf("payload too short for XOR-FEC metadata")
	}

	// Extract blockID (4 bytes, signed)
	blockID = int(int32(wrappedPayload[0])<<24 | int32(wrappedPayload[1])<<16 | int32(wrappedPayload[2])<<8 | int32(wrappedPayload[3]))

	// Extract seqInBlock (4 bytes, signed to handle -1 for parity)
	seqInBlock = int(int32(wrappedPayload[4])<<24 | int32(wrappedPayload[5])<<16 | int32(wrappedPayload[6])<<8 | int32(wrappedPayload[7]))

	// Extract isParity (1 byte)
	isParity = wrappedPayload[8] == 1

	// Extract payload
	payload = wrappedPayload[9:]

	return blockID, seqInBlock, isParity, payload, nil
}

func (fcs *XORCounterSubscriber) parseCounterValue(payload []byte) (uint64, error) {
	if len(payload) == 0 {
		return 0, fmt.Errorf("empty payload")
	}

	counterStr := string(payload)
	counterValue, err := strconv.ParseUint(counterStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid counter format '%s': %w", counterStr, err)
	}

	return counterValue, nil
}

func (fcs *XORCounterSubscriber) GetLastReceived() uint64 {
	fcs.mutex.RLock()
	defer fcs.mutex.RUnlock()
	return fcs.lastReceived
}

func (fcs *XORCounterSubscriber) GetTotalReceived() uint64 {
	fcs.mutex.RLock()
	defer fcs.mutex.RUnlock()
	return fcs.totalReceived
}

func (fcs *XORCounterSubscriber) GetRecoveredCount() uint64 {
	fcs.mutex.RLock()
	defer fcs.mutex.RUnlock()
	return fcs.recoveredCount
}

func (fcs *XORCounterSubscriber) IsRunning() bool {
	fcs.mutex.RLock()
	defer fcs.mutex.RUnlock()
	return fcs.isRunning
}
