// Command mole is the client half: run it NEXT TO your local service, on the
// machine WITHOUT a public address.
//
// It dials OUT to moled, authenticates with a token under a chosen name
// (becoming e.g. alice.example.com), then binds every incoming stream to a
// fresh connection to the local service.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
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

	for {
		runOnce()
		log.Printf("tunnel gone; redialing %s in %s", *relay, *retryWait)
		time.Sleep(*retryWait)
	}
}

func runOnce() {
	nc, err := net.Dial("tcp", *relay)
	if err != nil {
		log.Printf("dial %s failed: %v", *relay, err)
		time.Sleep(*retryWait)
		return
	}
	defer nc.Close()

	wc := wire.New(nc)

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
