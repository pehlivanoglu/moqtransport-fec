package main

import (
	"fmt"
	// "math/rand"
)

func main() {
	conf := Config{7, 3}
	enc := NewEncoder(conf)

	var parity, data [][]byte

	for i := 0; i < 7; i++ {
		packetData := []byte{byte(i /*rand.Intn(255)*/)}
		// moq.send(data)
		data, parity = enc.TryEncode(packetData)
	}

	fmt.Println("data:", data)
	fmt.Println("parity:", parity)

	shards := append(data, parity...)

	//Simulate error
	shards[1], shards[2], shards[0] = nil, nil, nil

	fmt.Println("with missing:", shards)

	dec := NewDecoder(conf)
	var recData, recParity [][]byte
	for _, v := range shards {
		recData, recParity = dec.TryDecode(v)
	}

	fmt.Println("after reconstruct:", recData, "parity: ", recParity)
}
