package main

import (
	"bytes"
	"sync"

	"github.com/klauspost/reedsolomon"
)

type Config struct {
	DataShards   int
	ParityShards int
}

type Encoder struct {
	rsEncoder reedsolomon.Encoder
	config    Config
	blockID   int
	seqID     int
	buffer    [][]byte
	mutex     sync.Mutex
}

func NewEncoder(config Config) *Encoder {
	enc, err := reedsolomon.New(config.DataShards, config.ParityShards)
	if err != nil {
		return nil
	}

	return &Encoder{
		rsEncoder: enc,
		config:    config,
		blockID:   0,
		seqID:     0,
		buffer:    make([][]byte, 0, config.DataShards),
	}
}

func (e *Encoder) TryEncode(payload []byte) (dataPackets [][]byte, parityPackets [][]byte) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	e.buffer = append(e.buffer, payload)
	e.seqID++

	if e.seqID < e.config.DataShards {
		return nil, nil
	}

	concat := bytes.Join(e.buffer, nil)
	shards, _ := e.rsEncoder.Split(concat)

	_ = e.rsEncoder.Encode(shards)
	_, _ = e.rsEncoder.Verify(shards) // Optional

	e.flushEncoder() // Caller holds the lock

	return shards[:e.config.DataShards], shards[e.config.DataShards:]
}

func (enc *Encoder) flushEncoder() {
	enc.buffer = make([][]byte, 0, enc.config.DataShards)
	enc.blockID++
	enc.seqID = 0
}

type Decoder struct {
	rsDecoder reedsolomon.Encoder
	config    Config
	blockID   int
	seqID     int
	buffer    [][]byte
	mutex     sync.Mutex
}

func NewDecoder(config Config) *Decoder {
	dec, err := reedsolomon.New(config.DataShards, config.ParityShards)
	if err != nil {
		return nil
	}

	return &Decoder{
		rsDecoder: dec,
		config:    config,
	}
}

func (d *Decoder) TryDecode(payload []byte) (dataPackets [][]byte, parityPackets [][]byte) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	d.buffer = append(d.buffer, payload)
	d.seqID++

	if d.seqID < d.config.DataShards+d.config.ParityShards {
		return nil, nil
	}

	_ = d.rsDecoder.Reconstruct(d.buffer)

	// ok, _ := d.rsDecoder.Verify(d.buffer)
	// if ok {
	// 	fmt.Println("ok")
	// }

	dataPackets = d.buffer

	d.flushDecoder()

	return dataPackets[:d.config.DataShards], dataPackets[d.config.DataShards:]
}

func (dec *Decoder) flushDecoder() {
	dec.buffer = make([][]byte, 0, dec.config.DataShards)
	dec.blockID++
	dec.seqID = 0
}
