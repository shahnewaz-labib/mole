package wire

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// dialPair wires two wire.Conns together over net.Pipe.
// NOTE: a Conn's Events() channel may have exactly ONE consumer.
func dialPair(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	ca, cb := New(a), New(b)
	t.Cleanup(func() { ca.Close(); cb.Close() })
	return ca, cb
}

// tap forwards every event from c onto a fresh channel for inspection.
func tap(t *testing.T, c *Conn) <-chan Event {
	t.Helper()
	events := make(chan Event, 64)
	go func() {
		for ev := range c.Events() {
			select {
			case events <- ev:
			default:
			}
		}
		close(events)
	}()
	return events
}

// serveEcho answers every incoming stream by echoing bytes back until EOF.
func serveEcho(c *Conn) {
	for ev := range c.Events() {
		if ev.Type != Syn {
			continue
		}
		go func(id uint64) {
			s, ok := c.Lookup(id)
			if !ok {
				return
			}
			buf := make([]byte, 4096)
			for {
				n, err := s.Read(buf)
				if n > 0 {
					if _, werr := s.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}(ev.ID)
	}
}

func waitEvent(t *testing.T, events <-chan Event, want FrameType) Event {
	t.Helper()
	select {
	case ev := <-events:
		if ev.Type != want {
			t.Fatalf("got event %v, want %v", ev.Type, want)
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event %v", want)
		return Event{}
	}
}

func TestEchoSingleStream(t *testing.T) {
	client, server := dialPair(t)
	go serveEcho(server)

	st, err := client.Open()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello through the mux")
	if _, err := st.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(st, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("echo mismatch: %q", got)
	}
}

func TestConcurrentStreams(t *testing.T) {
	client, server := dialPair(t)
	go serveEcho(server)

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := client.Open()
			if err != nil {
				t.Error(err)
				return
			}
			defer st.Close()

			payload := make([]byte, 200_000+i*137) // > chunkSize → multi-frame writes
			rand.Read(payload)
			go st.Write(payload)

			got := make([]byte, len(payload))
			if _, err := io.ReadFull(st, got); err != nil {
				t.Errorf("stream %d: %v", i, err)
				return
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("stream %d: payload mismatch", i)
			}
		}(i)
	}
	wg.Wait()
}

func TestCloseCausesEOF(t *testing.T) {
	client, server := dialPair(t)
	events := tap(t, server)

	st, err := client.Open()
	if err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, events, Syn)
	srv, ok := server.Lookup(ev.ID)
	if !ok {
		t.Fatal("server has no stream after SYN")
	}

	st.Close() // send FIN

	// Peer's read side must observe EOF once its buffer drains.
	deadline := time.Now().Add(2 * time.Second)
	buf := make([]byte, 16)
	for time.Now().Before(deadline) {
		_, err := srv.Read(buf)
		if err == io.EOF {
			return // success
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("never saw EOF after FIN")
}

func TestDeadConnEmitsEvent(t *testing.T) {
	a, b := net.Pipe()
	ca, cb := New(a), New(b)
	defer cb.Close()

	a.Close() // kill the transport underneath

	select {
	case ev := <-ca.Events():
		if ev.Type != Dead {
			t.Fatalf("got %v, want Dead", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Dead event")
	}
	if !ca.Dead() {
		t.Fatal("Dead() false after death")
	}
}
