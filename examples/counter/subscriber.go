package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"

	"github.com/mengelbart/moqtransport"
)

type CounterSubscriber struct {
	remoteTrack *moqtransport.RemoteTrack

	sessionID uint64
	namespace []string
	trackname string

	lastReceived  uint64
	totalReceived uint64
	isRunning     bool
	stopChan      chan struct{}

	mutex sync.RWMutex
}

func NewCounterSubscriber(remoteTrack *moqtransport.RemoteTrack, sessionID uint64, namespace []string, trackname string) *CounterSubscriber {
	return &CounterSubscriber{
		remoteTrack:   remoteTrack,
		sessionID:     sessionID,
		namespace:     namespace,
		trackname:     trackname,
		lastReceived:  0,
		totalReceived: 0,
		isRunning:     false,
		stopChan:      make(chan struct{}),
	}
}

func (cs *CounterSubscriber) Start(ctx context.Context) error {
	cs.mutex.Lock()
	if cs.isRunning {
		cs.mutex.Unlock()
		return fmt.Errorf("counter subscriber is already running")
	}
	cs.isRunning = true
	cs.mutex.Unlock()

	log.Printf("Starting counter subscriber (session: %d, namespace: %v, track: %s)",
		cs.sessionID, cs.namespace, cs.trackname)

	go cs.receiveLoop(ctx)

	select {
	case <-ctx.Done():
		log.Printf("Counter subscriber stopped due to context cancellation")
		return ctx.Err()
	case <-cs.stopChan:
		log.Printf("Counter subscriber stopped")
		return nil
	}
}

func (cs *CounterSubscriber) Stop() {
	cs.mutex.Lock()
	defer cs.mutex.Unlock()

	if !cs.isRunning {
		return
	}

	cs.isRunning = false
	close(cs.stopChan)
}

func (cs *CounterSubscriber) receiveLoop(ctx context.Context) {
	defer func() {
		cs.mutex.Lock()
		cs.isRunning = false
		cs.mutex.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-cs.stopChan:
			return
		default:
			obj, err := cs.remoteTrack.ReadObject(ctx)
			if err != nil {
				if err == io.EOF {
					log.Printf("Received last object - track ended")
					return
				}
				log.Printf("Error reading object: %v", err)
				return
			}

			if err := cs.processObject(obj); err != nil {
				log.Printf("Error processing object: %v", err)
				continue
			}
		}
	}
}

func (cs *CounterSubscriber) processObject(obj *moqtransport.Object) error {
	counterValue, err := cs.parseCounterValue(obj.Payload)
	if err != nil {
		return fmt.Errorf("failed to parse counter value: %w", err)
	}

	cs.mutex.Lock()
	cs.lastReceived = counterValue
	cs.totalReceived++
	cs.mutex.Unlock()

	log.Printf("Received MoQ-Datagram: %d",
		counterValue)

	return nil
}

func (cs *CounterSubscriber) parseCounterValue(payload []byte) (uint64, error) {
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

func (cs *CounterSubscriber) GetLastReceived() uint64 {
	cs.mutex.RLock()
	defer cs.mutex.RUnlock()
	return cs.lastReceived
}

func (cs *CounterSubscriber) GetTotalReceived() uint64 {
	cs.mutex.RLock()
	defer cs.mutex.RUnlock()
	return cs.totalReceived
}

func (cs *CounterSubscriber) IsRunning() bool {
	cs.mutex.RLock()
	defer cs.mutex.RUnlock()
	return cs.isRunning
}
