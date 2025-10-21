package quicmoq

import (
	"context"
	"log"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go/logging"
)

type LogLevel int

const (
	LogLevelSilent LogLevel = iota
	LogLevelError
	LogLevelWarn
	LogLevelInfo
	LogLevelDebug
	LogLevelTrace
)

type WindowMode int

const (
	ByTime WindowMode = iota
	ByPackets
)

// default rolling window used when WindowMode==ByTime
const defaultWindow = 10 * time.Second

type LoggerConfig struct {
	LogLevel          LogLevel
	MaxEventsPerType  int
	EnableTerminalLog bool
	Window            time.Duration // used when ByTime
	WindowMode        WindowMode    // ByTime or ByPackets
	WindowPackets     int           // used when ByPackets, e.g., 200
}

type AckEvent struct {
	Timestamp       time.Time
	PacketNumber    logging.PacketNumber
	EncryptionLevel logging.EncryptionLevel
	DelayTime       time.Duration
	AckRanges       []logging.AckRange
}

type LossEvent struct {
	Timestamp       time.Time
	PacketNumber    logging.PacketNumber
	EncryptionLevel logging.EncryptionLevel
	Reason          logging.PacketLossReason
}

type ConnectionEvent struct {
	Timestamp time.Time
	EventType string
	Local     net.Addr
	Remote    net.Addr
	Error     error
}

type MOQObjectEvent struct {
	Timestamp  time.Time
	GroupID    uint64
	SubGroupID uint64
	ObjectID   uint64
	Size       int
	Direction  string // "sent" | "received"
	TrackName  string
	Namespace  []string
}

type stampCount struct {
	t time.Time
	n int64
}

type byteSample struct {
	t time.Time
	b int64
}

type pktState uint8

const (
	psUnknown pktState = iota
	psAcked
	psLost
)

type pktSample struct {
	t    time.Time
	num  logging.PacketNumber
	size int64
	st   pktState
}
type QuicLogger struct {
	// identity
	sessionID   string
	perspective logging.Perspective

	// settings
	logLevel   LogLevel
	window     time.Duration
	windowMode WindowMode
	winPkts    int

	// snapshots and events (optional)
	ackEvents        []AckEvent
	lossEvents       []LossEvent
	connectionEvents []ConnectionEvent
	moqObjectEvents  []MOQObjectEvent

	// rolling samples (protected by mtx)
	mtx sync.RWMutex

	// ByTime accounting
	sentPkts  []stampCount // count of packets sent
	rcvdPkts  []stampCount // count of packets received
	ackedPkts []stampCount // count of packets acked
	lostPkts  []stampCount // count of packets lost
	sentBytes []byteSample // bytes sent
	rcvdBytes []byteSample // bytes received

	// ByPackets accounting (cohort ring of the last N sent packets)
	ring      []pktSample
	ringHead  int
	ringCount int
	byNum     map[logging.PacketNumber]int // packet number -> index in ring

	// latest RTT from quic-go
	latestRTT time.Duration

	// smoothed telemetry (EWMA) to stabilize readings
	smoothedLoss float64
	smoothedRTT  float64 // nanoseconds

	start time.Time
}

func NewQuicLogger(sessionID string, perspective logging.Perspective, cfg *LoggerConfig) *QuicLogger {
	if cfg == nil {
		cfg = &LoggerConfig{
			LogLevel:          LogLevelInfo,
			MaxEventsPerType:  1000,
			EnableTerminalLog: true,
			Window:            defaultWindow,
			WindowMode:        ByPackets,
			WindowPackets:     100,
		}
	}
	w := cfg.Window
	if w <= 0 {
		w = defaultWindow
	}
	if cfg.WindowPackets <= 0 {
		cfg.WindowPackets = 100
	}
	return &QuicLogger{
		sessionID:        sessionID,
		perspective:      perspective,
		logLevel:         cfg.LogLevel,
		window:           w,
		windowMode:       cfg.WindowMode,
		winPkts:          cfg.WindowPackets,
		start:            time.Now(),
		smoothedLoss:     0,
		smoothedRTT:      0,
		ackEvents:        make([]AckEvent, 0, cfg.MaxEventsPerType),
		lossEvents:       make([]LossEvent, 0, cfg.MaxEventsPerType),
		connectionEvents: make([]ConnectionEvent, 0, cfg.MaxEventsPerType),
		moqObjectEvents:  make([]MOQObjectEvent, 0, cfg.MaxEventsPerType),
	}
}

func trimBytesN(s []byteSample, n int) []byteSample {
	if n <= 0 {
		return s[:0]
	}
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func (q *QuicLogger) prune(now time.Time) {
	// Always prune time-based slices so they don't grow without bound.
	cut := now.Add(-q.window)
	pruneStamps := func(s []stampCount) []stampCount {
		i := 0
		for i < len(s) && s[i].t.Before(cut) {
			i++
		}
		return s[i:]
	}
	pruneBytes := func(s []byteSample) []byteSample {
		i := 0
		for i < len(s) && s[i].t.Before(cut) {
			i++
		}
		return s[i:]
	}
	q.sentPkts = pruneStamps(q.sentPkts)
	q.rcvdPkts = pruneStamps(q.rcvdPkts)
	q.ackedPkts = pruneStamps(q.ackedPkts)
	q.lostPkts = pruneStamps(q.lostPkts)
	q.sentBytes = pruneBytes(q.sentBytes)
	q.rcvdBytes = pruneBytes(q.rcvdBytes)

	// In packet-window mode, also cap rcvdBytes to ~2x window packets to bound memory.
	if q.windowMode == ByPackets && len(q.rcvdBytes) > 2*q.winPkts {
		q.rcvdBytes = trimBytesN(q.rcvdBytes, 2*q.winPkts)
	}
}

// ByTime adds
func (q *QuicLogger) addSent(size logging.ByteCount) {
	now := time.Now()
	q.sentPkts = append(q.sentPkts, stampCount{t: now, n: 1})
	q.sentBytes = append(q.sentBytes, byteSample{t: now, b: int64(size)})
	q.prune(now)
}
func (q *QuicLogger) addRcvd(size logging.ByteCount) {
	now := time.Now()
	q.rcvdPkts = append(q.rcvdPkts, stampCount{t: now, n: 1})
	q.rcvdBytes = append(q.rcvdBytes, byteSample{t: now, b: int64(size)})
	q.prune(now)
}

// ByPackets cohort ops
func (q *QuicLogger) ringEnsure() {
	if q.ring == nil {
		q.ring = make([]pktSample, q.winPkts)
		q.byNum = make(map[logging.PacketNumber]int, q.winPkts)
	}
}

func (q *QuicLogger) onSent(number logging.PacketNumber, size logging.ByteCount) {
	q.mtx.Lock()
	defer q.mtx.Unlock()

	if q.windowMode == ByPackets {
		q.ringEnsure()
		idx := q.ringHead
		// evict old
		if q.ringCount == q.winPkts {
			old := q.ring[idx]
			delete(q.byNum, old.num)
		} else {
			q.ringCount++
		}
		q.ring[idx] = pktSample{t: time.Now(), num: number, size: int64(size), st: psUnknown}
		q.byNum[number] = idx
		q.ringHead = (q.ringHead + 1) % q.winPkts
		// also keep a small receive buffer bounded
		q.prune(time.Now())
		return
	}

	// ByTime
	q.addSent(size)
}

func (q *QuicLogger) onRcvd(size logging.ByteCount) {
	q.mtx.Lock()
	defer q.mtx.Unlock()

	// Track RX bytes for both modes to compute RX rate.
	q.addRcvd(size)
}

func (q *QuicLogger) onAck(number logging.PacketNumber) {
	q.mtx.Lock()
	defer q.mtx.Unlock()

	if q.windowMode == ByPackets {
		if idx, ok := q.byNum[number]; ok {
			q.ring[idx].st = psAcked // overrides prior psLost
		}
		return
	}

	// ByTime
	q.ackedPkts = append(q.ackedPkts, stampCount{t: time.Now(), n: 1})
	q.prune(time.Now())
}

func (q *QuicLogger) onLoss(number logging.PacketNumber) {
	q.mtx.Lock()
	defer q.mtx.Unlock()

	if q.windowMode == ByPackets {
		if idx, ok := q.byNum[number]; ok && q.ring[idx].st == psUnknown {
			q.ring[idx].st = psLost
		}
		return
	}

	// ByTime
	q.lostPkts = append(q.lostPkts, stampCount{t: time.Now(), n: 1})
	q.prune(time.Now())
}

func (q *QuicLogger) logToTerminal(level LogLevel, format string, args ...interface{}) {
	if level <= q.logLevel {
		outArgs := make([]any, 0, 1+len(args))
		outArgs = append(outArgs, q.sessionID)
		outArgs = append(outArgs, args...)
		log.Printf("[%s] "+format, outArgs...)
	}
}

type RollingMetrics struct {
	Window       time.Duration
	PktWindow    int // when ByPackets, the cohort size
	SentPackets  int64
	AckedPackets int64
	LostPackets  int64
	RecvPackets  int64
	SentBytes    int64
	RecvBytes    int64
	LossPercent  float64
	SendRateBps  float64
	RecvRateBps  float64
	SmoothedRTT  time.Duration
	AsOf         time.Time
}

func sumStamps(s []stampCount) int64 {
	var n int64
	for _, x := range s {
		n += x.n
	}
	return n
}
func sumBytes(s []byteSample) int64 {
	var n int64
	for _, x := range s {
		n += x.b
	}
	return n
}

func (q *QuicLogger) packetWindowMetrics() (sent, acked, lost int64, bytes int64, span time.Duration) {
	if q.ringCount == 0 {
		return
	}
	// ringHead points to next insertion position.
	// iterate last ringCount samples in chronological order.
	firstIdx := -1
	lastIdx := -1
	for i := 0; i < q.ringCount; i++ {
		idx := (q.ringHead - q.ringCount + i + q.winPkts) % q.winPkts
		s := q.ring[idx]
		// slots may be zeroed before fully populated
		if s.num == 0 && s.size == 0 && s.t.IsZero() {
			continue
		}
		if firstIdx == -1 {
			firstIdx = idx
		}
		lastIdx = idx
		sent++
		bytes += s.size
		switch s.st {
		case psAcked:
			acked++
		case psLost:
			lost++
		}
	}
	if firstIdx >= 0 && lastIdx >= 0 {
		t0 := q.ring[firstIdx].t
		t1 := q.ring[lastIdx].t
		if t1.After(t0) {
			span = t1.Sub(t0)
		} else {
			span = time.Second
		}
	} else {
		span = time.Second
	}
	return
}

func (q *QuicLogger) recvBytesWithin(span time.Duration) (bytes int64, packets int64) {
	if span <= 0 {
		return 0, 0
	}
	cut := time.Now().Add(-span)
	for _, s := range q.rcvdBytes {
		if s.t.After(cut) || s.t.Equal(cut) {
			bytes += s.b
		}
	}
	for _, s := range q.rcvdPkts {
		if s.t.After(cut) || s.t.Equal(cut) {
			packets += s.n
		}
	}
	return
}

func (q *QuicLogger) GetRollingMetrics() RollingMetrics {
	now := time.Now()

	q.mtx.RLock()
	defer q.mtx.RUnlock()

	// RTT output
	outRTT := q.latestRTT
	if q.smoothedRTT > 0 {
		outRTT = time.Duration(uint64(q.smoothedRTT))
	}

	if q.windowMode == ByPackets {
		sentPk, ackPk, lostPk, sentBy, span := q.packetWindowMetrics()
		if span <= 0 {
			span = time.Second
		}
		loss := 0.0
		if sentPk > 0 {
			loss = float64(lostPk) * 100.0 / float64(sentPk)
		}
		// EWMA to stabilize loss
		alpha := 0.5
		if q.smoothedLoss == 0 {
			q.smoothedLoss = loss
		} else {
			q.smoothedLoss = alpha*loss + (1-alpha)*q.smoothedLoss
		}
		sendRate := float64(sentBy) * 8.0 / span.Seconds()
		rxBytes, rxPkts := q.recvBytesWithin(span)
		recvRate := float64(rxBytes) * 8.0 / span.Seconds()

		return RollingMetrics{
			Window:       span,
			PktWindow:    q.winPkts,
			SentPackets:  sentPk,
			AckedPackets: ackPk,
			LostPackets:  lostPk,
			RecvPackets:  rxPkts,
			SentBytes:    sentBy,
			RecvBytes:    rxBytes,
			LossPercent:  q.smoothedLoss,
			SendRateBps:  sendRate,
			RecvRateBps:  recvRate,
			SmoothedRTT:  outRTT,
			AsOf:         now,
		}
	}

	// ByTime mode
	sentPk := sumStamps(q.sentPkts)
	ackPk := sumStamps(q.ackedPkts)
	lostPk := sumStamps(q.lostPkts)
	rcvPk := sumStamps(q.rcvdPkts)
	sentBy := sumBytes(q.sentBytes)
	rcvBy := sumBytes(q.rcvdBytes)

	loss := 0.0
	if sentPk > 0 {
		loss = float64(lostPk) * 100.0 / float64(sentPk)
	}
	sendRate := float64(sentBy) * 8.0 / q.window.Seconds()
	recvRate := float64(rcvBy) * 8.0 / q.window.Seconds()

	alpha := 0.2
	if q.smoothedLoss == 0 {
		q.smoothedLoss = loss
	} else {
		q.smoothedLoss = alpha*loss + (1-alpha)*q.smoothedLoss
	}

	return RollingMetrics{
		Window:       q.window,
		PktWindow:    0,
		SentPackets:  sentPk,
		AckedPackets: ackPk,
		LostPackets:  lostPk,
		RecvPackets:  rcvPk,
		SentBytes:    sentBy,
		RecvBytes:    rcvBy,
		LossPercent:  q.smoothedLoss,
		SendRateBps:  sendRate,
		RecvRateBps:  recvRate,
		SmoothedRTT:  outRTT,
		AsOf:         now,
	}
}

func (q *QuicLogger) GetRTT() time.Duration {
	q.mtx.RLock()
	defer q.mtx.RUnlock()
	return q.latestRTT
}

// Optional: expose events if needed
func (q *QuicLogger) RecordMOQObject(groupID, subGroupID, objectID uint64, size int, direction, trackName string, namespace []string) {
	q.mtx.Lock()
	q.moqObjectEvents = append(q.moqObjectEvents, MOQObjectEvent{
		Timestamp:  time.Now(),
		GroupID:    groupID,
		SubGroupID: subGroupID,
		ObjectID:   objectID,
		Size:       size,
		Direction:  direction,
		TrackName:  trackName,
		Namespace:  namespace,
	})
	q.mtx.Unlock()
}

func (q *QuicLogger) CreateConnectionTracer() func(context.Context, logging.Perspective, logging.ConnectionID) *logging.ConnectionTracer {
	return func(ctx context.Context, p logging.Perspective, _ logging.ConnectionID) *logging.ConnectionTracer {
		q.perspective = p

		return &logging.ConnectionTracer{
			StartedConnection: func(local, remote net.Addr, srcConnID, destConnID logging.ConnectionID) {
				q.mtx.Lock()
				q.connectionEvents = append(q.connectionEvents, ConnectionEvent{
					Timestamp: time.Now(),
					EventType: "started",
					Local:     local,
					Remote:    remote,
				})
				q.mtx.Unlock()
				q.logToTerminal(LogLevelInfo, "conn started local=%v remote=%v", local, remote)
			},
			ClosedConnection: func(err error) {
				q.mtx.Lock()
				q.connectionEvents = append(q.connectionEvents, ConnectionEvent{
					Timestamp: time.Now(),
					EventType: "closed",
					Error:     err,
				})
				q.mtx.Unlock()
				q.logToTerminal(LogLevelInfo, "conn closed err=%v", err)
			},
			SentLongHeaderPacket: func(hdr *logging.ExtendedHeader, size logging.ByteCount, ecn logging.ECN, ack *logging.AckFrame, frames []logging.Frame) {
				q.onSent(hdr.PacketNumber, size)
			},
			SentShortHeaderPacket: func(hdr *logging.ShortHeader, size logging.ByteCount, ecn logging.ECN, ack *logging.AckFrame, frames []logging.Frame) {
				q.onSent(hdr.PacketNumber, size)
			},
			ReceivedLongHeaderPacket: func(hdr *logging.ExtendedHeader, size logging.ByteCount, ecn logging.ECN, frames []logging.Frame) {
				q.onRcvd(size)
			},
			ReceivedShortHeaderPacket: func(hdr *logging.ShortHeader, size logging.ByteCount, ecn logging.ECN, frames []logging.Frame) {
				q.onRcvd(size)
			},
			AcknowledgedPacket: func(level logging.EncryptionLevel, number logging.PacketNumber) {
				q.onAck(number)
			},
			LostPacket: func(level logging.EncryptionLevel, number logging.PacketNumber, reason logging.PacketLossReason) {
				q.onLoss(number)
			},
			UpdatedMetrics: func(rttStats *logging.RTTStats, cwnd, bytesInFlight logging.ByteCount, packetsInFlight int) {
				q.mtx.Lock()
				q.latestRTT = rttStats.SmoothedRTT()
				alpha := 0.2
				rttNs := float64(q.latestRTT.Nanoseconds())
				if q.smoothedRTT == 0 {
					q.smoothedRTT = rttNs
				} else {
					q.smoothedRTT = alpha*rttNs + (1-alpha)*q.smoothedRTT
				}
				q.prune(time.Now())
				q.mtx.Unlock()
				q.logToTerminal(LogLevelDebug, "rtt=%v cwnd=%d bif=%d pif=%d", q.latestRTT, cwnd, bytesInFlight, packetsInFlight)
			},
			Close: func() {},
		}
	}
}

func (q *QuicLogger) SetLogLevel(level LogLevel) {
	q.mtx.Lock()
	q.logLevel = level
	q.mtx.Unlock()
}

func (q *QuicLogger) LogSummary() {
	m := q.GetRollingMetrics()
	if q.windowMode == ByPackets {
		log.Printf("=== QUIC %s Summary (pkts=%d span=%v) ===", q.sessionID, m.PktWindow, m.Window)
	} else {
		log.Printf("=== QUIC %s Summary (window=%v) ===", q.sessionID, m.Window)
	}
	log.Printf("RTT=%v Loss=%.2f%% SentPkts=%d Acked=%d Lost=%d SentRate=%.1f kbps RecvRate=%.1f kbps",
		m.SmoothedRTT, m.LossPercent, m.SentPackets, m.AckedPackets, m.LostPackets, m.SendRateBps/1000, m.RecvRateBps/1000)
	log.Printf("====================================")
}

func (q *QuicLogger) LogDetailedStats() {
	q.LogSummary()
}
