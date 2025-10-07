package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/mengelbart/moqtransport"
)

type CounterPublisher struct {
	publisher moqtransport.Publisher

	sessionID uint64
	requestID uint64

	counter   uint64
	groupID   uint64
	isRunning bool
	stopChan  chan struct{}

	mutex sync.RWMutex
}

func New(publisher moqtransport.Publisher, sessionID, requestID uint64) *CounterPublisher {
	return &CounterPublisher{
		publisher: publisher,
		sessionID: sessionID,
		requestID: requestID,
		counter:   0,
		groupID:   0,
		isRunning: false,
		stopChan:  make(chan struct{}),
	}
}

func (cp *CounterPublisher) Start(ctx context.Context) error {
	cp.mutex.Lock()
	if cp.isRunning {
		cp.mutex.Unlock()
		return fmt.Errorf("counter publisher is already running")
	}
	cp.isRunning = true
	cp.mutex.Unlock()

	log.Printf("Starting counter publisher (session: %d, request: %d)", cp.sessionID, cp.requestID)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("Counter publisher stopped due to context cancellation")
			return ctx.Err()

		case <-cp.stopChan:
			log.Printf("Counter publisher stopped")
			return nil

		case <-ticker.C:
			if err := cp.publishNext(); err != nil {
				log.Printf("Failed to publish counter value %d: %v", cp.counter, err)
				return err
			}
		}
	}
}

func (cp *CounterPublisher) Stop() {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	if !cp.isRunning {
		return
	}

	cp.isRunning = false
	close(cp.stopChan)
}

func (cp *CounterPublisher) publishNext() error {
	cp.mutex.Lock()
	currentCounter := cp.counter
	currentGroupID := cp.groupID

	cp.counter++
	cp.groupID++
	cp.mutex.Unlock()

	payload := []byte(strconv.FormatUint(currentCounter, 10))

	obj := moqtransport.Object{
		GroupID:              currentGroupID,
		ObjectID:             0,              // Single object per group for simplicity
		SubGroupID:           currentGroupID, // Same as GroupID according to moqt-draft
		Payload:              payload,
		ForwardingPreference: moqtransport.ObjectForwardingPreferenceDatagram,
	}

	if err := cp.publisher.SendDatagram(obj); err != nil {
		return fmt.Errorf("failed to send datagram for counter %d: %w", currentCounter, err)
	}

	log.Printf("Published MoQ-Datagram: %d",
		currentCounter)

	return nil
}

func (cp *CounterPublisher) GetCurrentCounter() uint64 {
	cp.mutex.RLock()
	defer cp.mutex.RUnlock()
	return cp.counter
}

func (cp *CounterPublisher) IsRunning() bool {
	cp.mutex.RLock()
	defer cp.mutex.RUnlock()
	return cp.isRunning
}

func (cp *CounterPublisher) CloseWithError(code uint64, reason string) error {
	log.Printf("Closing counter publisher with error (code: %d, reason: %s)", code, reason)
	cp.Stop()
	return cp.publisher.CloseWithError(code, reason)
}

func (cp *CounterPublisher) SendDatagram(obj moqtransport.Object) error {
	return cp.publisher.SendDatagram(obj)
}

func (cp *CounterPublisher) OpenSubgroup(groupID, subgroupID uint64, priority uint8) (*moqtransport.Subgroup, error) {
	// NOTE: This counter publisher only uses datagrams, subgroups are not needed.
	// This method is only implemented to satisfy the moqtransport.Publisher interface.
	log.Printf("Warning: OpenSubgroup called on datagram-only publisher (session: %d, request: %d)",
		cp.sessionID, cp.requestID)
	return cp.publisher.OpenSubgroup(groupID, subgroupID, priority)
}
