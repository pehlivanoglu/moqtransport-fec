package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"sync"
	"sync/atomic"

	"github.com/mengelbart/moqtransport"
	"github.com/mengelbart/moqtransport/quicmoq"
	"github.com/quic-go/quic-go"
)

const (
	appName = "counter"
)

var usg = `%s acts either as a MoQ server or client with a track
that publishes/subscribes to counter data.
In both cases it can publish or subscribe to a track.

Usage of %s:
`

type options struct {
	certFile  string
	keyFile   string
	addr      string
	server    bool
	publish   bool
	subscribe bool
	namespace string
	trackname string
}

type counterHandler struct {
	server        bool
	addr          string
	tlsConfig     *tls.Config
	namespace     []string
	trackname     string
	publish       bool
	subscribe     bool
	nextSessionID atomic.Uint64
	publishers    map[moqtransport.Publisher]struct{}
	lock          sync.Mutex
}

func parseOptions(fs *flag.FlagSet, args []string) (*options, error) {
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, usg, appName, appName)
		fmt.Fprintf(os.Stderr, "%s [options]\n\noptions:\n", appName)
		fs.PrintDefaults()
	}

	opts := options{}
	fs.StringVar(&opts.certFile, "cert", "localhost.pem", "TLS certificate file (only used for server)")
	fs.StringVar(&opts.keyFile, "key", "localhost-key.pem", "TLS key file (only used for server)")
	fs.StringVar(&opts.addr, "addr", "localhost:8080", "listen or connect address")
	fs.BoolVar(&opts.server, "server", false, "run as server")
	fs.BoolVar(&opts.publish, "publish", false, "publish a counter track")
	fs.BoolVar(&opts.subscribe, "subscribe", false, "subscribe to a counter track")
	fs.StringVar(&opts.namespace, "namespace", "counter", "Namespace to subscribe to")
	fs.StringVar(&opts.trackname, "trackname", "count", "Track to subscribe to")
	err := fs.Parse(args[1:])
	return &opts, err
}

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	opts, err := parseOptions(fs, args)

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if opts.server {
		return runServer(opts)
	}
	return runClient(opts)
}

func runServer(opts *options) error {
	tlsConfig, err := generateTLSConfigWithCertAndKey(opts.certFile, opts.keyFile)
	if err != nil {
		log.Printf("failed to generate TLS config from cert file and key, generating in memory certs: %v", err)
		tlsConfig, err = generateTLSConfig()
		if err != nil {
			log.Fatal(err)
		}
	}
	h := &counterHandler{
		server:     true,
		addr:       opts.addr,
		tlsConfig:  tlsConfig,
		namespace:  []string{opts.namespace},
		trackname:  opts.trackname,
		publish:    opts.publish,
		subscribe:  false,
		publishers: make(map[moqtransport.Publisher]struct{}),
	}
	return h.runServer(context.TODO())
}

func runClient(opts *options) error {
	h := &counterHandler{
		server:     false,
		addr:       opts.addr,
		tlsConfig:  nil,
		namespace:  []string{opts.namespace},
		trackname:  opts.trackname,
		publish:    false,
		subscribe:  opts.subscribe,
		publishers: nil,
	}
	return h.runClient(context.TODO())
}

func generateTLSConfigWithCertAndKey(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"moq-00"},
	}, nil
}

// Setup a bare-bones TLS config for the server
func generateTLSConfig() (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1)}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"moq-00"},
	}, nil
}

func dialQUIC(ctx context.Context, addr string) (moqtransport.Connection, error) {
	conn, err := quic.DialAddr(ctx, addr, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"moq-00"},
	}, &quic.Config{
		EnableDatagrams: true,
	})
	if err != nil {
		return nil, err
	}
	return quicmoq.NewClient(conn), nil
}

func (h *counterHandler) runClient(ctx context.Context) error {
	conn, err := dialQUIC(ctx, h.addr)
	if err != nil {
		return err
	}
	if err = h.handle(conn); err != nil {
		return err
	}
	select {}
}

func (h *counterHandler) runServer(ctx context.Context) error {
	listener, err := quic.ListenAddr(h.addr, h.tlsConfig, &quic.Config{
		EnableDatagrams: true,
	})
	if err != nil {
		return err
	}

	log.Printf("MOQ server listening on %s", h.addr)

	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			return err
		}
		if conn.ConnectionState().TLS.NegotiatedProtocol == "moq-00" {
			go h.handle(quicmoq.NewServer(conn))
		}
	}
}

func (h *counterHandler) getHandler(sessionID uint64) moqtransport.Handler {
	return moqtransport.HandlerFunc(func(w moqtransport.ResponseWriter, r *moqtransport.Message) {
		switch r.Method {
		case moqtransport.MessageAnnounce:
			if !h.subscribe {
				log.Printf("sessionNr: %d got unexpected announcement: %v", sessionID, r.Namespace)
				w.Reject(0, "counter doesn't take announcements")
				return
			}
			if !tupleEqual(r.Namespace, h.namespace) {
				log.Printf("got unexpected announcement namespace: %v, expected %v", r.Namespace, h.namespace)
				w.Reject(0, "non-matching namespace")
				return
			}
			err := w.Accept()
			if err != nil {
				log.Printf("failed to accept announcement: %v", err)
				return
			}
		}
	})
}

func (h *counterHandler) getSubscribeHandler(sessionID uint64) moqtransport.SubscribeHandler {
	return moqtransport.SubscribeHandlerFunc(func(w *moqtransport.SubscribeResponseWriter, m *moqtransport.SubscribeMessage) {
		if !h.publish {
			log.Printf("sessionNr: %d got unexpected subscribe request: %v", sessionID, m.Namespace)
			w.Reject(moqtransport.ErrorCodeSubscribeTrackDoesNotExist, "endpoint does not publish any tracks")
			return
		}
		if !tupleEqual(m.Namespace, h.namespace) || m.Track != h.trackname {
			log.Printf("got unexpected subscribe namespace/track: %v/%v, expected %v/%v", m.Namespace, m.Track, h.namespace, h.trackname)
			w.Reject(moqtransport.ErrorCodeSubscribeTrackDoesNotExist, "unknown track")
			return
		}
		err := w.Accept()
		if err != nil {
			log.Printf("failed to accept subscription: %v", err)
			return
		}
		log.Printf("sessionNr: %d accepted subscription for namespace %v track %v", sessionID, m.Namespace, m.Track)
		counterPublisher := NewXORPublisher(w, sessionID, m.RequestID)
		h.lock.Lock()
		h.publishers[counterPublisher] = struct{}{}
		h.lock.Unlock()

		// Start the counter publisher
		go func() {
			if err := counterPublisher.Start(context.Background()); err != nil {
				log.Printf("Counter publisher stopped with error: %v", err)
			}
		}()
	})
}

func (h *counterHandler) handle(conn moqtransport.Connection) error {
	id := h.nextSessionID.Add(1)
	session := &moqtransport.Session{
		Handler:             h.getHandler(id),
		SubscribeHandler:    h.getSubscribeHandler(id),
		InitialMaxRequestID: 100,
	}
	if err := session.Run(conn); err != nil {
		return err
	}
	if h.publish {
		if err := session.Announce(context.Background(), h.namespace); err != nil {
			log.Printf("failed to announce namespace '%v': %v", h.namespace, err)
		}
	}
	if h.subscribe {
		if err := h.subscribeAndRead(session, h.namespace, h.trackname); err != nil {
			return err
		}
	}
	return nil
}

func (h *counterHandler) subscribeAndRead(s *moqtransport.Session, namespace []string, trackname string) error {
	rs, err := s.Subscribe(context.Background(), namespace, trackname)
	if err != nil {
		return err
	}

	// Create a counter subscriber to handle the received data
	subscriber := NewXORSubscriber(rs, h.nextSessionID.Load(), namespace, trackname)
	go func() {
		if err := subscriber.Start(context.Background()); err != nil {
			log.Printf("Counter subscriber stopped with error: %v", err)
		}
	}()

	return nil
}

func tupleEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, t := range a {
		if t != b[i] {
			return false
		}
	}
	return true
}
