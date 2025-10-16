package xor

import (
	"sync"
)

type Config struct {
	BlockSize int
}

type Encoder struct {
	config  Config
	blockID int
	seqID   int
	buffer  [][]byte
	mutex   sync.Mutex
}

func NewEncoder(config Config) *Encoder {
	return &Encoder{
		config:  config,
		blockID: 0,
		seqID:   0,
		buffer:  make([][]byte, 0, config.BlockSize),
	}
}

func (e *Encoder) TryEncode(payload []byte) (parityPacket []byte, blockID int, seqID int) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	e.buffer = append(e.buffer, payload)
	e.seqID++

	blockID = e.blockID
	seqID = e.seqID

	if e.seqID < e.config.BlockSize {
		return nil, blockID, seqID
	}

	parityData := e.generateParity(e.buffer)

	e.flushEncoder()

	return parityData, blockID, seqID
}

func (e *Encoder) generateParity(packets [][]byte) []byte {
	if len(packets) == 0 {
		return nil
	}

	maxLen := 0
	for _, packet := range packets {
		if len(packet) > maxLen {
			maxLen = len(packet)
		}
	}

	parity := make([]byte, maxLen)
	for _, packet := range packets {
		for i := 0; i < len(packet); i++ {
			parity[i] ^= packet[i]
		}
	}

	return parity
}

func (enc *Encoder) flushEncoder() {
	enc.buffer = make([][]byte, 0, enc.config.BlockSize)
	enc.blockID++
	enc.seqID = 0
}

type Block struct {
	dataPackets  map[int][]byte // seq -> payload
	parityPacket []byte
	receivedData int
	hasParity    bool
	size         int
}

type Decoder struct {
	config Config
	blocks map[int]*Block
	mutex  sync.RWMutex
}

func NewDecoder(config Config) *Decoder {
	return &Decoder{
		config: config,
		blocks: make(map[int]*Block),
	}
}

func (d *Decoder) TryDecode(blockID int, seqInBlock int, payload []byte, isParity bool) ([]byte, bool) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	block, exists := d.blocks[blockID]
	if !exists {
		block = &Block{
			dataPackets: make(map[int][]byte),
			size:        d.config.BlockSize,
		}
		d.blocks[blockID] = block
	}

	if isParity {
		block.parityPacket = payload
		block.hasParity = true
	} else {
		if _, exists := block.dataPackets[seqInBlock]; !exists {
			block.dataPackets[seqInBlock] = payload
			block.receivedData++
		}
	}

	// Check for recover
	if block.receivedData == block.size {
		delete(d.blocks, blockID)
		return payload, false
	}

	// Try recovery if have parity and missing exactly one packet
	if block.hasParity && block.receivedData == block.size-1 {
		recovered := d.recoverMissingPacket(block)
		if recovered != nil {
			delete(d.blocks, blockID)
			return recovered, true
		}
	}

	if !isParity {
		return payload, false
	}

	return nil, false
}

// Recover the missing packet via XOR
func (d *Decoder) recoverMissingPacket(block *Block) []byte {
	if !block.hasParity || block.receivedData != block.size-1 {
		return nil
	}

	// Find missing sequence num
	missingSeq := -1
	for i := 0; i < block.size; i++ {
		if _, exists := block.dataPackets[i]; !exists {
			missingSeq = i
			break
		}
	}

	if missingSeq == -1 {
		return nil
	}

	// Start with parity packet
	recovered := make([]byte, len(block.parityPacket))
	copy(recovered, block.parityPacket)

	// XOR with all received data packets
	for _, packet := range block.dataPackets {
		minLen := len(recovered)
		if len(packet) < minLen {
			minLen = len(packet)
		}
		for i := 0; i < minLen; i++ {
			recovered[i] ^= packet[i]
		}
	}

	return recovered
}

func (d *Decoder) CleanupOldBlocks(currentBlockID int) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	for blockID := range d.blocks {
		if blockID < currentBlockID-10 { // Keep last 10 blocks for safety
			delete(d.blocks, blockID)
		}
	}
}
