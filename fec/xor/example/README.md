# XOR FEC Counter Example

A MoQ Transport example demonstrating XOR-based Forward Error Correction (FEC) with counter data.

## Features

- **XOR FEC**: Automatic packet loss recovery using parity packets
- **Real-time counter**: Publishes incrementing numbers every second
- **Recovery logging**: Shows when packets are recovered vs. received normally

## Quick Start

**Start the server (publishes counter with FEC):**
```bash
go run . -server -publish
```

**Start the client (subscribes and recovers lost packets):**
```bash
go run . -subscribe
```

## How it Works

1. **Publisher**: Sends counter values (0, 1, 2, 3...) with FEC encoding
2. **FEC blocks**: Every 4 data packets + 1 parity packet for recovery
3. **Subscriber**: Receives packets and automatically recovers lost ones
4. **Logging**: Shows `(RECOVERED)` when FEC successfully restores missing data

## Options

```bash
go run . -help
```

**Example output:**
```
Received MoQ-Datagram: 5, blockID=1, seqInBlock=1
Received MoQ-Datagram: 7, blockID=1, seqInBlock=3  
Received MoQ-Datagram: 6, blockID=1, seqInBlock=PARITY (RECOVERED)
```