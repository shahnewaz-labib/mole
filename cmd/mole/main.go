// Command mole is the client half: run it NEXT TO your local service, on the
// machine WITHOUT a public address.
//
// It dials OUT to moled, authenticates with a token under a chosen name
// (becoming e.g. alice.example.com), then binds every incoming stream to a
// fresh connection to the local service.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"time"

	"mole/internal/wire"
)

var (
	relay     = flag.String("relay", "localhost:7000", "address of moled's tunnel port (host:port)")
	localAddr = flag.String("local", "localhost:8000", "local service to expose")
	name      = flag.String("name", "", "tunnel name (default: this machine's hostname)")
	token     = flag.String("token", "", "auth token expected by moled (required)")
	retryWait = flag.Duration("retry-wait", 2*time.Second, "pause between relay dial attempts")

	useTLS  = flag.Bool("tls", false, "dial the relay over TLS")
	caFile  = flag.String("ca", "", "CA bundle (PEM) to verify the relay; empty = system roots")
	tlsName = flag.String("tls-name", "", "server name for TLS verification (default: host part of --relay)")
)

// authMsg / authAck mirror the structs in cmd/moled.
type authMsg struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

func main() {
	flag.Parse()
	if *token == "" {
		log.Fatal("--token is required")
	}
	if *name == "" {
		h, err := os.Hostname()
		if err != nil {
			log.Fatal("--name is required (hostname unavailable): ", err)
		}
		*name = sanitize(h)
	}

	// Exponential backoff with jitter. The counter resets only when the
	// relay ACKs our auth — a dial that succeeds but then dies instantly
	// must not let us hammer the server.
	const maxWait = 30 * time.Second
	wait := *retryWait
	for {
		authed := false
		runOnce(func() { authed = true })
		if authed {
			wait = *retryWait
		} else {
			wait *= 2
			if wait > maxWait {
				wait = maxWait
			}
		}
		jitter := time.Duration(rand.Int64N(int64(wait) / 4))
		log.Printf("tunnel gone; redialing %s in ~%s", *relay, wait+jitter)
		time.Sleep(wait + jitter)
	}
}

// runOnce maintains one multiplexed tunnel until it dies, calling markAuth
// once the relay accepts our credentials.
func runOnce(markAuth func()) {
	nc, err := dialRelay()
	if err != nil {
		log.Printf("dial %s failed: %v", *relay, err)
		time.Sleep(*retryWait)
		return
	}
	defer nc.Close()

	wc := wire.New(nc, wire.WithKeepalive(wire.PingInterval, wire.PingTimeout))

	auth, _ := json.Marshal(authMsg{Name: *name, Token: *token})
	if err := wc.Control(wire.Auth, auth); err != nil {
		log.Printf("auth send failed: %v", err)
		return
	}

	for ev := range wc.Events() {
		switch ev.Type {
		case wire.AuthAck:
			var ack struct {
				Domain string `json:"domain"`
			}
			_ = json.Unmarshal(ev.Body, &ack)
			if ack.Domain != "" {
				log.Printf("tunnel live: https://%s.%s -> %s", *name, ack.Domain, *localAddr)
			} else {
				log.Printf("tunnel live: name=%q -> %s (visitors must set Host: %s)",
					*name, *localAddr, *name)
			}
			markAuth()

		case wire.Reject:
			// A rejection will not heal by retrying — fail loudly instead.
			log.Fatalf("relay rejected us: %s", string(ev.Body))

		case wire.Syn:
			go bind(wc, ev.ID)

		case wire.Fin:
			if s, ok := wc.Lookup(ev.ID); ok {
				s.Close() // release local readers/writers promptly
			}
		}
	}
}

// dialRelay connects to the relay, wrapping in TLS when --tls is set.
func dialRelay() (net.Conn, error) {
	if !*useTLS {
		return net.Dial("tcp", *relay)
	}
	host, _, err := net.SplitHostPort(*relay)
	if err != nil {
		host = *relay
	}
	sni := *tlsName
	if sni == "" {
		sni = host
	}
	cfg := &tls.Config{ServerName: sni, NextProtos: []string{"mole/1"}}
	if *caFile != "" {
		pemBytes, err := os.ReadFile(*caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, errors.New("--ca file contains no PEM certificates")
		}
		cfg.RootCAs = pool
	} else if net.ParseIP(sni) != nil {
		return nil, errors.New("dialing an IP over TLS needs verification: " +
			"pass --tls-name or --ca with a matching cert")
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	return tls.DialWithDialer(&d, "tcp", *relay, cfg)
}

// bind connects one incoming stream to the local service and pumps bytes
// both ways until either side finishes.
func bind(wc *wire.Conn, id uint64) {
	st, ok := wc.Lookup(id)
	if !ok {
		return
	}
	svc, err := net.Dial("tcp", *localAddr)
	if err != nil {
		log.Printf("stream %d: local service %s unreachable: %v", id, *localAddr, err)
		st.Close()
		return
	}

	log.Printf("stream %d -> %s", id, *localAddr)

	go io.Copy(svc, st) // request bytes → local service
	io.Copy(st, svc)    // response bytes → visitor
	st.Close()
	svc.Close()
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return -1
		}
	}, strings.ReplaceAll(s, " ", "-"))
	return strings.Trim(s, "-")
}
