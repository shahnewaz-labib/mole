package main

import (
	"crypto/tls"
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
	catchAll = flag.String("default", "", "catch-all tunnel name: requests whose Host matches "+
		"nothing go here. Lets visitors use plain http://VPS_IP with no domain at all.")
	tunnelCert = flag.String("tunnel-cert", "", "TLS certificate for the tunnel port (enables TLS)")
	tunnelKey  = flag.String("tunnel-key", "", "TLS private key for the tunnel port")
	publicCert = flag.String("public-cert", "", "TLS certificate for the public port (enables HTTPS)")
	publicKey  = flag.String("public-key", "", "TLS private key for the public port")
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

	tunLn, err := net.Listen("tcp", *tunnelAddr)
	if err != nil {
		log.Fatal(err)
	}
	if *tunnelCert != "" {
		cert, err := tls.LoadX509KeyPair(*tunnelCert, *tunnelKey)
		if err != nil {
			log.Fatal(err)
		}
		tunLn = tls.NewListener(tunLn, &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"mole/1"},
		})
		log.Print("tunnel door speaks TLS")
	}
	go acceptTunnels(tunLn, reg)

	srv := &http.Server{
		Addr:              *publicAddr,
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveVisitorHTTP(w, r, reg) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	scheme := "HTTP"
	if *publicCert != "" {
		scheme = "HTTPS"
	}
	log.Printf("moled up: visitors %s (%s) · tunnels %s · domain %q · catch-all %q",
		*publicAddr, scheme, *tunnelAddr, *rootDomain, *catchAll)
	if *publicCert != "" {
		log.Fatal(srv.ListenAndServeTLS(*publicCert, *publicKey))
	} else {
		log.Fatal(srv.ListenAndServe())
	}
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
	wc := wire.New(nc, wire.WithKeepalive(wire.PingInterval, wire.PingTimeout))

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

// resolveTunnel decides where a visitor request goes: by Host header when it
// names a live tunnel, otherwise the catch-all --default tunnel (for
// IP-only deployments with no domain).
func resolveTunnel(r *http.Request, reg *registry) (*wire.Conn, string) {
	host := hostOnly(r.Host)

	var name string
	switch {
	case *rootDomain == "":
		name = host // exact-Host mode
	case strings.HasSuffix(host, "."+*rootDomain):
		name = strings.TrimSuffix(host, "."+*rootDomain)
	}
	if name != "" && validName(name) {
		if be := reg.pick(name); be != nil {
			return be, name
		}
	}

	if *catchAll != "" {
		if be := reg.pick(*catchAll); be != nil {
			return be, *catchAll
		}
	}
	return nil, name
}

func serveVisitorHTTP(w http.ResponseWriter, r *http.Request, reg *registry) {
	be, name := resolveTunnel(r, reg)
	if be == nil {
		log.Printf("visitor %s: no live tunnel for host %q", r.RemoteAddr, hostOnly(r.Host))
		notFound(w)
		return
	}
	log.Printf("visitor %s -> tunnel %q", r.RemoteAddr, name)
	httputilProxy(be).ServeHTTP(w, r)
}

func hostOnly(h string) string {
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h, "]") {
		h = h[:i]
	} else if i := strings.LastIndex(h, "]:"); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}
