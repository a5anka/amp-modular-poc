package modhost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Broadcaster fans out server-sent events to connected consoles.
type Broadcaster struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

// NewBroadcaster creates a broadcaster.
func NewBroadcaster() *Broadcaster { return &Broadcaster{clients: map[chan string]struct{}{}} }

// Send pushes an event to every connected console.
func (b *Broadcaster) Send(kind string, data any) {
	payload, _ := json.Marshal(data)
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", kind, payload)
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.clients {
		select {
		case c <- msg:
		default: // slow client: drop rather than block the core
		}
	}
}

// ServeHTTP streams events to one console.
func (b *Broadcaster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	c := make(chan string, 16)
	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.clients, c)
		b.mu.Unlock()
	}()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-c:
			fmt.Fprint(w, msg)
			flusher.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
