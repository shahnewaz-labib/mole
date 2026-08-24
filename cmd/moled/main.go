package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"mole/internal/wire"
)

var (
	publicAddr = flag.String("public-addr", ":8080", "listen address for visitor traffic")
	tunnelAddr = flag.String("tunnel-addr", ":7000", "listen address for tunnel connections")
	authToken  = flag.String("auth-token", "", "token clients must present (required)")
	rootDomain = flag.String("domain", "", "root domain for tunnels, e.g. example.com — "+
		"visitors reach a tunnel at <name>.<domain>. Empty = match exact Host headers.")
)

// authMsg / authAck mirror the structs in cmd/mole (kept tiny on purpose).
type authMsg struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

func main() {
	flag.Parse()
	if *authToken == "" {
		log.Fatal("--auth-token is required (refusing to run an open relay)")
	}

	reg := newRegistry()

	tun, err := net.Listen("tcp", *tunnelAddr)
	if err != nil {
		log.Fatal(err)
	}
	go acceptTunnels(tun, reg)

	srv := &http.Server{
		Addr:              *publicAddr,
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveVisitorHTTP(w, r, reg) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("moled up: visitors %s · tunnels %s · domain %q",
		*publicAddr, *tunnelAddr, *rootDomain)
	log.Fatal(srv.ListenAndServe())
}

// acceptTunnels collects outbound client dials; each must authenticate as its
// first frame before it can carry any traffic.
func acceptTunnels(l net.Listener, reg *registry) {
	for {
		nc, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go handleTunnel(nc, reg)
	}
}

func handleTunnel(nc net.Conn, reg *registry) {
	wc := wire.New(nc)

	// The very first event must be an Auth control frame.
	var authed authMsg
	select {
	case ev, ok := <-wc.Events():
		if !ok {
			return
		}
		if ev.Type != wire.Auth {
			rejectTunnel(wc, "first frame must be auth")
			return
		}
		if err := json.Unmarshal(ev.Body, &authed); err != nil {
			rejectTunnel(wc, "malformed auth")
			return
		}
	case <-time.After(10 * time.Second):
		log.Printf("tunnel %s timed out before auth", nc.RemoteAddr())
		nc.Close()
		return
	}

	if authed.Token != *authToken {
		log.Printf("tunnel %q rejected: bad token", authed.Name)
		rejectTunnel(wc, "bad token")
		return
	}
	if !validName(authed.Name) {
		rejectTunnel(wc, "invalid tunnel name")
		return
	}

	reg.add(authed.Name, wc)

	ack, _ := json.Marshal(map[string]string{"domain": *rootDomain})
	if err := wc.Control(wire.AuthAck, ack); err != nil {
		return
	}
	log.Printf("tunnel %q registered from %s", authed.Name, nc.RemoteAddr())

	for range wc.Events() {
		// SYN/Data/etc. from a client are meaningless (clients never open
		// streams); drain until the connection dies.
	}

	reg.remove(authed.Name, wc)
	log.Printf("tunnel %q disconnected", authed.Name)
}

func rejectTunnel(wc *wire.Conn, reason string) {
	_ = wc.Control(wire.Reject, []byte(reason))
	wc.Close()
}

func validName(name string) bool {
	return nameRegexp.MatchString(name)
}

var nameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// serveVisitorHTTP routes one visitor request to the right tunnel by Host.
func serveVisitorHTTP(w http.ResponseWriter, r *http.Request, reg *registry) {
	host := hostOnly(r.Host)

	name := host
	if *rootDomain != "" {
		suffix := "." + *rootDomain
		if !strings.HasSuffix(host, suffix) {
			notFound(w)
			return
		}
		name = strings.TrimSuffix(host, suffix)
	}
	if !validName(name) {
		notFound(w)
		return
	}

	be := reg.pick(name)
	if be == nil {
		log.Printf("visitor %s: no live tunnel named %q", r.RemoteAddr, name)
		notFound(w)
		return
	}

	rp := httputilProxy(be)
	rp.ServeHTTP(w, r)
}

func hostOnly(h string) string {
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h, "]") {
		h = h[:i]
	} else if i := strings.LastIndex(h, "]:"); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}
