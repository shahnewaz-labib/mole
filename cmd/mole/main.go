// Command mole is the client half: run it NEXT TO your local service, on the
// machine WITHOUT a public address (your laptop). It dials OUT to moled — the
// direction every firewall allows — and keeps spare connections parked there.
// When a visitor's first byte arrives on one, it pipes that connection to the
// local service. Your laptop never listens for anything.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"time"
)

var (
	relay    = flag.String("relay", "localhost:7000", "address of moled's tunnel port (host:port)")
	local    = flag.String("local", "localhost:8000", "local service to expose")
	poolSize = flag.Int("pool", 8, "how many spare tunnel connections to keep parked")
)

func main() {
	flag.Parse()
	for i := 0; i < *poolSize; i++ {
		go keepParked() // each goroutine maintains one parked connection forever
	}
	log.Printf("mole up: %d tunnels → %s, exposing %s", *poolSize, *relay, *local)
	select {} // sleep the main goroutine forever; the workers do everything
}

// keepParked is one worker's endless life: dial the relay, park the
// connection, get consumed by a visitor, repeat. If the relay is down, back
// off and try again — self-healing with zero human intervention.
func keepParked() {
	for {
		tun, err := net.Dial("tcp", *relay)
		if err != nil {
			log.Printf("dial %s failed (%v); retrying in 2s", *relay, err)
			time.Sleep(2 * time.Second)
			continue
		}
		serve(tun) // blocks until this connection is used up or breaks
	}
}

// serve waits silently for PROOF that a visitor arrived — the first byte —
// then splices the tunnel connection to the local service.
func serve(tun net.Conn) {
	defer tun.Close()

	buf := make([]byte, 4096)
	n, err := tun.Read(buf) // blocks until moled forwards the visitor's first bytes
	if err != nil {
		return // relay died or dropped us; keepParked loops and redials
	}

	svc, err := net.Dial("tcp", *local) // note: can't name this `local` — it would shadow the *string flag
	if err != nil {
		log.Printf("local service %s unreachable: %v", *local, err)
		return
	}
	defer svc.Close()
	svc.Write(buf[:n]) // deliver the bytes we already read

	go io.Copy(svc, tun) // rest of the request → laptop
	io.Copy(tun, svc)    // response → visitor (blocks until done)
}
