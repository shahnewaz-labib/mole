package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"mole/internal/wire"
)

var (
	publicAddr = flag.String("public-addr", ":8080", "listen address for visitor traffic")
	tunnelAddr = flag.String("tunnel-addr", ":7000", "listen address for tunnel connections")
	authToken  = flag.String("auth-token", "", "token clients must present; empty = load "+
		"from ~/.moled/token or generate one there automatically")
	rootDomain = flag.String("domain", "", "root domain for tunnels, e.g. example.com — "+
		"visitors reach a tunnel at <name>.<domain>. Empty = match exact Host headers.")
	catchAll = flag.String("default", "", "catch-all tunnel name: requests whose Host matches "+
		"nothing go here. Lets visitors use plain http://VPS_IP with no domain at all.")
	portMap = flag.String("port-map", "", "per-tunnel public ports, e.g. \"alice=8081,bob=8082\": "+
		"each listed port serves exactly that tunnel regardless of Host (domain-free multi-service)")
	advertise = flag.String("advertise", "", "public hostname/IP shown in the suggested client "+
		"command (cosmetic only — default placeholder <your-vps-ip>)")
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

// authAckMsg tells the client everything it needs to print a visitor URL.
type authAckMsg struct {
	Domain string `json:"domain"` // root domain, empty = none
	Host   string `json:"host"`   // --advertise value, empty = unknown
	Port   string `json:"port"`   // mapped public port for this tunnel, no colon
	Scheme string `json:"scheme"` // "https" only when relay serves HTTPS itself
}

// acceptOpts carries what handleTunnel needs beyond the registry.
type acceptOpts struct {
	reg         *registry
	mappedPorts map[string]string // tunnel name -> ":port"
	publicHTTPS bool
}

func main() {
	flag.Parse()

	token, tokenSource := resolveAuthToken()
	*authToken = token // handleTunnel compares against this

	if *advertise == "" {
		if ip := detectPublicIP(); ip != "" {
			log.Printf("detected public IP %s (override anytime with --advertise)", ip)
			*advertise = ip
		}
	}

	reg := newRegistry()

	// Domain-free multi-service mode: one public port per named tunnel.
	// mappedPorts also feeds the auth ack so clients can print their URL.
	mappedPorts := make(map[string]string)
	if *portMap != "" {
		for _, entry := range strings.Split(*portMap, ",") {
			parts := strings.SplitN(strings.TrimSpace(entry), "=", 2)
			if len(parts) != 2 {
				log.Fatalf("bad --port-map entry %q (want name=port)", entry)
			}
			name := strings.TrimSpace(parts[0])
			addr := ":" + strings.TrimPrefix(strings.TrimSpace(parts[1]), ":")
			if !validName(name) {
				log.Fatalf("bad --port-map tunnel name %q", name)
			}
			mappedPorts[name] = addr
			go func(name, addr string) {
				mux := http.NewServeMux()
				mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
					serveFixedTunnel(w, r, reg, name)
				})
				srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
				log.Printf("mapped port %s -> tunnel %q", addr, name)
				log.Fatal(srv.ListenAndServe())
			}(name, addr)
		}
	}

	// Hint comes after parsing so commands can be complete and runnable.
	printConnectHint(token, tokenSource, mappedPorts)

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
	go acceptTunnels(tunLn, acceptOpts{
		reg:         reg,
		mappedPorts: mappedPorts,
		publicHTTPS: *publicCert != "",
	})

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
func acceptTunnels(l net.Listener, opts acceptOpts) {
	for {
		nc, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go handleTunnel(nc, opts)
	}
}

func handleTunnel(nc net.Conn, opts acceptOpts) {
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

	opts.reg.add(authed.Name, wc)

	ack := authAckMsg{
		Domain: *rootDomain,
		Host:   *advertise,
		Port:   strings.TrimPrefix(opts.mappedPorts[authed.Name], ":"),
		Scheme: "http",
	}
	if opts.publicHTTPS {
		ack.Scheme = "https"
	}
	ackBytes, _ := json.Marshal(ack)
	if err := wc.Control(wire.AuthAck, ackBytes); err != nil {
		return
	}
	log.Printf("tunnel %q registered from %s", authed.Name, nc.RemoteAddr())

	for range wc.Events() {
		// SYN/Data/etc. from a client are meaningless (clients never open
		// streams); drain until the connection dies.
	}

	opts.reg.remove(authed.Name, wc)
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

// serveFixedTunnel sends everything arriving on a mapped port to one named
// tunnel, ignoring the Host header entirely.
func serveFixedTunnel(w http.ResponseWriter, r *http.Request, reg *registry, name string) {
	be := reg.pick(name)
	if be == nil {
		log.Printf("visitor %s -> tunnel %q: not connected (mapped port)", r.RemoteAddr, name)
		serviceUnavailable(w, name)
		return
	}
	log.Printf("visitor %s -> tunnel %q (mapped port)", r.RemoteAddr, name)
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

// tokenFilePath returns where the relay persists its auth token.
func tokenFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".moled", "token"), nil
}

// resolveAuthToken determines the relay's shared secret, in order of
// preference: --auth-token flag > ~/.moled/token > generate & persist.
// The second return value records where the token came from.
func resolveAuthToken() (string, string) {
	if *authToken != "" {
		return *authToken, "flag"
	}

	p, pErr := tokenFilePath()
	if pErr == nil {
		if b, rErr := os.ReadFile(p); rErr == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				log.Printf("using auth token from %s", p)
				return t, "file"
			}
		}
	}

	t := genToken()
	if pErr != nil {
		log.Printf("generated ephemeral auth token for this run (%v)", pErr)
		return t, "ephemeral"
	}
	if mkErr := os.MkdirAll(filepath.Dir(p), 0o700); mkErr != nil {
		log.Printf("generated ephemeral auth token for this run (%v)", mkErr)
		return t, "ephemeral"
	}
	if wErr := os.WriteFile(p, []byte(t+"\n"), 0o600); wErr != nil {
		log.Printf("generated ephemeral auth token for this run (%v)", wErr)
		return t, "ephemeral"
	}
	log.Printf("generated auth token, saved to %s (mode 0600)", p)
	return t, "generated"
}

func genToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("entropy source unavailable: %v", err)
	}
	return hex.EncodeToString(b)
}

// detectPublicIP asks a series of well-known echo services what address our
// outbound traffic egresses from. On a typical VPS that IS the public IP
// (unlike interface addresses, which are often provider-NAT'd privates).
// Best-effort: any failure just leaves advertise unset. Override with
// --advertise when behind extra NAT or when a hostname should be shown.
func detectPublicIP() string {
	endpoints := []string{
		"https://api.ipify.org",
		"https://ifconfig.me/ip",
		"https://icanhazip.com",
	}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, u := range endpoints {
		resp, err := client.Get(u)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if net.ParseIP(ip) != nil {
			return ip
		}
	}
	return ""
}

// printConnectHint emits one COMPLETE, runnable client command per mapped
// tunnel — placeholders only where the operator truly must decide. Tokens
// passed via --auth-token are not echoed (logs get shipped; secrets shouldn't).
func printConnectHint(token, source string, mappedPorts map[string]string) {
	host := *advertise
	if host == "" {
		host = "<your-vps-ip>"
	}
	relayPort := strings.TrimPrefix(*tunnelAddr, ":")
	tok := token
	if source == "flag" {
		tok = "<the-token-you-passed-via-flag>"
	}

	names := make([]string, 0, len(mappedPorts))
	for n := range mappedPorts {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("\nClients can connect with:\n\n")
	if len(names) == 0 {
		fmt.Fprintf(&b,
			"    mole --relay=%s:%s --token=%s --name=pick-a-name --local=localhost:<port>\n"+
				"    # (--name matters when --domain / --default / --port-map is used)\n",
			host, relayPort, tok)
	} else {
		for _, n := range names {
			fmt.Fprintf(&b,
				"    mole --relay=%s:%s --token=%s --name=%s --local=localhost:<port>\n",
				host, relayPort, tok, n)
			fmt.Fprintf(&b, "        -> visitors open http://%s%s\n", host, mappedPorts[n])
		}
		b.WriteString("\n(edit <port> to wherever your service listens; " +
			"multiple tunnels? add pairs to --port-map and run more clients)\n")
	}
	fmt.Print(b.String())
}
