package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
)

// portAlloc hands out stable public ports to tunnel names.
//
// Sources of truth, in order:
//  1. static --port-map entries (operator-pinned, never reallocated)
//  2. previously assigned ranges persisted in portsFile (~/.moled/ports.json)
//  3. fresh picks from the configured range, lowest-first
//
// Persisting matters: without it, a moled restart could hand a different
// port to an existing tunnel name and silently break everyone's bookmarks.
type portAlloc struct {
	mu     sync.Mutex
	start  int
	end    int
	used   map[int]string
	byName map[string]int
	path   string // "" = don't persist (tests)

	// onStart must bind the public listener; returning an error makes the
	// allocator treat the port as unusable and try the next one.
	onStart func(name, addr string) error
}

func newPortAlloc(start, end int, path string, onStart func(name, addr string) error) *portAlloc {
	pa := &portAlloc{
		start:  start,
		end:    end,
		used:   make(map[int]string),
		byName: make(map[string]int),
		path:   path,
		onStart: onStart,
	}
	pa.load()
	return pa
}

type portFile map[string]int

func (pa *portAlloc) load() {
	if pa.path == "" {
		return
	}
	b, err := os.ReadFile(pa.path)
	if err != nil {
		return // first run
	}
	var pf portFile
	if err := json.Unmarshal(b, &pf); err != nil {
		log.Printf("warning: ignoring unreadable %s: %v", pa.path, err)
		return
	}
	for name, port := range pf {
		pa.used[port] = name
		pa.byName[name] = port
	}
}

func (pa *portAlloc) save() {
	if pa.path == "" {
		return
	}
	b, _ := json.MarshalIndent(portFile(pa.byName), "", "  ")
	if err := os.WriteFile(pa.path, b, 0o600); err != nil {
		log.Printf("warning: could not persist port assignments: %v", err)
	}
}

// ReserveStatic pins an operator-chosen mapping so the range skips it.
func (pa *portAlloc) ReserveStatic(name string, port int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	pa.used[port] = name
	pa.byName[name] = port
}

// Ensure returns the port for name, assigning (and announcing) a new one
// from the range if needed.
func (pa *portAlloc) Ensure(name string) (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	if p, ok := pa.byName[name]; ok {
		return p, nil
	}
	for port := pa.start; port <= pa.end; port++ {
		if _, taken := pa.used[port]; taken {
			continue
		}
		addr := fmt.Sprintf(":%d", port)
		if err := pa.onStart(name, addr); err != nil {
			continue // something external owns this port; try the next
		}
		pa.used[port] = name
		pa.byName[name] = port
		pa.save()
		log.Printf("auto-assigned port %s -> tunnel %q", addr, name)
		return port, nil
	}
	return 0, fmt.Errorf("no free ports left in range %d-%d", pa.start, pa.end)
}
