# Counter Example

A simple example app that publishes/subscribes to incrementing counter data using MoQ datagrams.

## Usage

**Run server (publisher):**
```bash
go run . -server -publish
```

**Run client (subscriber):**
```bash
go run . -subscribe
```

**For more options:**
```bash
go run . -h
```