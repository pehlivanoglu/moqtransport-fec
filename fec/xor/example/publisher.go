package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/mengelbart/moqtransport"
	"github.com/mengelbart/moqtransport/fec/xor"
)

type XORCounterPublisher struct {
	publisher moqtransport.Publisher
	encoder   *xor.Encoder

	sessionID uint64
	requestID uint64

	counter   uint64
	groupID   uint64
	isRunning bool
	stopChan  chan struct{}

	mutex sync.RWMutex
}

func NewXORPublisher(publisher moqtransport.Publisher, sessionID, requestID uint64) *XORCounterPublisher {
	config := xor.Config{BlockSize: 4}
	return &XORCounterPublisher{
		publisher: publisher,
		encoder:   xor.NewEncoder(config),
		sessionID: sessionID,
		requestID: requestID,
		counter:   0,
		groupID:   0,
		isRunning: false,
		stopChan:  make(chan struct{}),
	}
}

func (fcp *XORCounterPublisher) Start(ctx context.Context) error {
	fcp.mutex.Lock()
	if fcp.isRunning {
		fcp.mutex.Unlock()
		return fmt.Errorf("XOR-FEC counter publisher is already running")
	}
	fcp.isRunning = true
	fcp.mutex.Unlock()

	log.Printf("Starting XOR-FEC counter publisher (session: %d, request: %d)", fcp.sessionID, fcp.requestID)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("XOR-FEC counter publisher stopped due to context cancellation")
			return ctx.Err()

		case <-fcp.stopChan:
			log.Printf("XOR-FEC counter publisher stopped")
			return nil

		case <-ticker.C:
			if err := fcp.publishNext(); err != nil {
				log.Printf("Failed to publish counter value %d: %v", fcp.counter, err)
				return err
			}
		}
	}
}

func (fcp *XORCounterPublisher) publishNext() error {
	fcp.mutex.Lock()
	currentCounter := fcp.counter
	currentGroupID := fcp.groupID

	fcp.counter++
	fcp.groupID++
	fcp.mutex.Unlock()

	payload := []byte(strconv.FormatUint(currentCounter, 10))

	parityPacket, blockID, seqID := fcp.encoder.TryEncode(payload)

	obj := moqtransport.Object{
		GroupID:              currentGroupID,
		ObjectID:             0,
		SubGroupID:           currentGroupID,
		Payload:              fcp.wrapWithMetadata(payload, blockID, seqID, false),
		ForwardingPreference: moqtransport.ObjectForwardingPreferenceDatagram,
	}

	if err := fcp.publisher.SendDatagram(obj); err != nil {
		return fmt.Errorf("failed to send data packet %d: %w", obj, err)
	}
	log.Printf("Published: counter=%d, blockID=%d, seqInBlock=%d", currentCounter, blockID, seqID)

	if parityPacket != nil {
		parityObj := moqtransport.Object{
			GroupID:              currentGroupID + 1000, // Use different GroupID for parity
			ObjectID:             0,
			SubGroupID:           currentGroupID + 1000,
			Payload:              fcp.wrapWithMetadata(parityPacket, blockID, -1, true),
			ForwardingPreference: moqtransport.ObjectForwardingPreferenceDatagram,
		}

		if err := fcp.publisher.SendDatagram(parityObj); err != nil {
			return fmt.Errorf("failed to send parity packet: %w", err)
		}
		log.Printf("Published parity: blockID=%d", blockID)
	}

	return nil
}

// Add FEC metadata to the payload
func (fcp *XORCounterPublisher) wrapWithMetadata(payload []byte, blockID, seqInBlock int, isParity bool) []byte {
	// [blockID:4][seqInBlock:4][isParity:1][payload...]
	metadata := make([]byte, 9+len(payload))

	// BlockID (4 bytes)
	metadata[0] = byte(blockID >> 24)
	metadata[1] = byte(blockID >> 16)
	metadata[2] = byte(blockID >> 8)
	metadata[3] = byte(blockID)

	// SeqInBlock (4 bytes)
	metadata[4] = byte(seqInBlock >> 24)
	metadata[5] = byte(seqInBlock >> 16)
	metadata[6] = byte(seqInBlock >> 8)
	metadata[7] = byte(seqInBlock)

	// IsParity (1 byte)
	if isParity {
		metadata[8] = 1
	} else {
		metadata[8] = 0
	}

	// Payload
	copy(metadata[9:], payload)

	return metadata
}

func (fcp *XORCounterPublisher) Stop() {
	fcp.mutex.Lock()
	defer fcp.mutex.Unlock()

	if !fcp.isRunning {
		return
	}

	fcp.isRunning = false
	close(fcp.stopChan)
}

func (fcp *XORCounterPublisher) CloseWithError(code uint64, reason string) error {
	log.Printf("Closing XOR-FEC counter publisher with error (code: %d, reason: %s)", code, reason)
	fcp.Stop()
	return fcp.publisher.CloseWithError(code, reason)
}

func (fcp *XORCounterPublisher) SendDatagram(obj moqtransport.Object) error {
	return fcp.publisher.SendDatagram(obj)
}

func (fcp *XORCounterPublisher) OpenSubgroup(groupID, subgroupID uint64, priority uint8) (*moqtransport.Subgroup, error) {
	return fcp.publisher.OpenSubgroup(groupID, subgroupID, priority)
}
