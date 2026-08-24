// Command mole is the client half: run it NEXT TO your local service, on the
// machine WITHOUT a public address.
//
// It dials OUT to moled once and multiplexes every visitor over that single
// connection using internal/wire streams. When a stream opens (a visitor
// arrived), it binds that stream to a fresh connection to the local service.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"mole/internal/wire"
)

var (
	relay     = flag.String("relay", "localhost:7000", "address of moled's tunnel port (host:port)")
	localAddr = flag.String("local", "localhost:8000", "local service to expose")
	retryWait = flag.Duration("retry-wait", 2*time.Second, "pause between relay dial attempts")
)

func main() {
	flag.Parse()
	for {
		runOnce()
		log.Printf("tunnel gone; redialing %s in %s", *relay, *retryWait)
		time.Sleep(*retryWait)
	}
}

// runOnce maintains one multiplexed tunnel until it dies.
func runOnce() {
	nc, err := net.Dial("tcp", *relay)
	if err != nil {
		log.Printf("dial %s failed: %v", *relay, err)
		time.Sleep(*retryWait)
		return
	}
	defer nc.Close()

	wc := wire.New(nc)
	log.Printf("tunnel established to %s (multiplexed)", *relay)

	// Track live visitor streams -> their local service connections, so a
	// FIN from the relay tears down the right local socket promptly.
	var (
		lmu    sync.Mutex
		locals = make(map[uint64]net.Conn)
	)

	for ev := range wc.Events() {
		switch ev.Type {
		case wire.Syn:
			go bind(wc, ev.ID, locals, &lmu)

		case wire.Fin:
			lmu.Lock()
			if svc, ok := locals[ev.ID]; ok {
				delete(locals, ev.ID)
				svc.Close() // visitor left: end the local conversation too
			}
			lmu.Unlock()

		case wire.Reject:
			log.Printf("rejected by relay: %s", string(ev.Body))
			return
		}
	}
}

// bind connects one incoming stream to the local service and pumps bytes
// both ways until either side finishes.
func bind(wc *wire.Conn, id uint64, locals map[uint64]net.Conn, lmu *sync.Mutex) {
	st, ok := wc.Lookup(id)
	if !ok {
		return
	}

	svc, err := net.Dial("tcp", *localAddr)
	if err != nil {
		log.Printf("stream %d: local service %s unreachable: %v", id, *localAddr, err)
		st.Close() // tell the relay we're done; visitor sees an empty reply
		return
	}

	lmu.Lock()
	locals[id] = svc
	lmu.Unlock()

	log.Printf("stream %d -> %s", id, *localAddr)

	go io.Copy(svc, st) // request bytes → local service
	io.Copy(st, svc)    // response bytes → visitor

	lmu.Lock()
	delete(locals, id)
	lmu.Unlock()
	st.Close()
	svc.Close()
}
