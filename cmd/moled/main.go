// Command moled is the relay half of mole: the daemon that runs on the
// machine WITH a public IP (your VPS). Visitors connect to it; it hands their
// bytes to parked tunnel connections that mole clients dialed OUT to it.
//
// Lesson 0001's inversion, implemented: moled never dials your laptop.
// It can't — your laptop has no routable address. It only listens, and every
// byte it sends "inbound" travels through a connection a client started.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"sync"
)

var (
	publicAddr = flag.String("public-addr", ":8080", "listen address for visitor traffic")
	tunnelAddr = flag.String("tunnel-addr", ":7000", "listen address for tunnel connections")
)

// Idle tunnel connections waiting for a visitor. Guarded by mu because the
// accept-loop goroutine appends while per-visitor goroutines take from it.
var (
	mu     sync.Mutex
	parked []net.Conn
)

func main() {
	flag.Parse()

	// Two doors:
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
		go serveVisitor(visitor) // one goroutine per visitor
	}
}

// acceptTunnels collects the connections clients dialed out and parks them.
func acceptTunnels(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		mu.Lock()
		parked = append(parked, conn)
		n := len(parked)
		mu.Unlock()
		log.Printf("tunnel parked from %s (idle pool=%d)", conn.RemoteAddr(), n)
	}
}

// serveVisitor pairs one visitor with one parked tunnel connection and splices
// them into a raw byte pipe. HTTP is just bytes on a stream — neither side of
// this function needs to understand it.
func serveVisitor(visitor net.Conn) {
	defer visitor.Close()

	mu.Lock()
	if len(parked) == 0 {
		mu.Unlock()
		log.Printf("visitor %s refused: no tunnel parked", visitor.RemoteAddr())
		return
	}
	tun := parked[0]
	parked = parked[1:]
	mu.Unlock()
	defer tun.Close() // connection is consumed; the client will dial a replacement

	log.Printf("splicing visitor %s <-> tunnel", visitor.RemoteAddr())

	done := make(chan struct{}, 2)
	go func() { io.Copy(tun, visitor); done <- struct{}{} }() // visitor → laptop
	go func() { io.Copy(visitor, tun); done <- struct{}{} }() // laptop → visitor
	<-done // whenever either direction ends, the defers close both conns,
	// which unblocks the other io.Copy too. One splice, fully torn down.
}
