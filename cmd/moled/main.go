// Command moled is the relay half of mole: the daemon that runs on the
// machine WITH a public IP (your VPS).
//
// Since the multiplexing milestone, ONE tunnel connection carries every
// visitor concurrently: each visitor gets a virtual stream (see
// internal/wire) spliced to their TCP connection. The relay still speaks no
// HTTP — it moves raw bytes.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"sync"

	"mole/internal/wire"
)

var (
	publicAddr = flag.String("public-addr", ":8080", "listen address for visitor traffic")
	tunnelAddr = flag.String("tunnel-addr", ":7000", "listen address for tunnel connections")
)

// The single active tunnel connection. Serving exactly one client is a
// documented v1 limitation; named multi-client routing comes next.
var (
	tmu    sync.Mutex
	tunnel *wire.Conn
)

func main() {
	flag.Parse()

	pub, err := net.Listen("tcp", *publicAddr) // public door — visitors arrive here
	if err != nil {
		log.Fatal(err)
	}
	tun, err := net.Listen("tcp", *tunnelAddr) // tunnel door — clients dial OUT to this
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("moled up: visitors %s · tunnels %s", *publicAddr, *tunnelAddr)

	go acceptTunnels(tun)

	for {
		visitor, err := pub.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go serveVisitor(visitor)
	}
}

func acceptTunnels(l net.Listener) {
	for {
		nc, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		tmu.Lock()
		if tunnel != nil {
			tmu.Unlock()
			log.Printf("extra tunnel from %s rejected: already serving one client", nc.RemoteAddr())
			nc.Close()
			continue
		}
		t := wire.New(nc)
		tunnel = t
		tmu.Unlock()
		log.Printf("tunnel connected from %s — every visitor now multiplexes over it", nc.RemoteAddr())
		go monitor(t)
	}
}

func monitor(t *wire.Conn) {
	for ev := range t.Events() {
		switch ev.Type {
		case wire.Syn:
			log.Print("unexpected SYN from tunnel client; ignoring")
		case wire.Reject:
			log.Printf("relay-side rejection: %s", ev.Body)
		}
	}
	tmu.Lock()
	if tunnel == t {
		tunnel = nil
	}
	tmu.Unlock()
	log.Print("tunnel connection lost")
}

func getTunnel() *wire.Conn {
	tmu.Lock()
	defer tmu.Unlock()
	return tunnel
}

func serveVisitor(visitor net.Conn) {
	defer visitor.Close()

	t := getTunnel()
	if t == nil || t.Dead() {
		log.Printf("visitor %s refused: no tunnel connected", visitor.RemoteAddr())
		return
	}

	st, err := t.Open()
	if err != nil {
		log.Printf("opening stream failed: %v", err)
		return
	}
	defer st.Close()

	log.Printf("splicing visitor %s <-> stream %d", visitor.RemoteAddr(), st.ID())

	done := make(chan struct{}, 2)
	go func() { io.Copy(st, visitor); done <- struct{}{} }() // visitor → laptop
	go func() { io.Copy(visitor, st); done <- struct{}{} }() // laptop → visitor
	<-done
}
