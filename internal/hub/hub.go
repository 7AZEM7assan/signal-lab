// Package hub fans messages out to WebSocket subscribers.
//
// Delivery is best-effort and live-only: nothing is buffered for absent
// clients and nothing is replayed on reconnect (use the HTTP query API to
// catch up). Slow-client policy: each client has a bounded send buffer, and a
// client whose buffer is full is DISCONNECTED rather than having messages
// dropped silently, so it can never observe a gap without knowing. Broadcast
// never blocks, so one slow client cannot stall ingestion or other clients.
package hub

import (
	"sync"

	"signallab/internal/metrics"
)

// Client is one subscriber. Consume Messages until Done is closed.
type Client struct {
	device string // empty = all devices
	send   chan []byte
	done   chan struct{}
	once   sync.Once
	reason string
}

// Messages yields encoded frames to write to the peer.
func (c *Client) Messages() <-chan []byte { return c.send }

// Done is closed when the hub has removed the client (slow consumer or shutdown).
func (c *Client) Done() <-chan struct{} { return c.done }

// Reason says why Done closed: "slow_consumer" or "shutdown".
func (c *Client) Reason() string { return c.reason }

type Hub struct {
	mu         sync.RWMutex
	clients    map[*Client]struct{}
	bufferSize int
	maxClients int
	m          *metrics.Metrics
	closed     bool
}

func New(bufferSize, maxClients int, m *metrics.Metrics) *Hub {
	return &Hub{clients: map[*Client]struct{}{}, bufferSize: bufferSize, maxClients: maxClients, m: m}
}

// Subscribe registers a client, or returns nil if the hub is full or closed.
// An empty device subscribes to every device.
func (h *Hub) Subscribe(device string) *Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.clients) >= h.maxClients {
		return nil
	}
	c := &Client{device: device, send: make(chan []byte, h.bufferSize), done: make(chan struct{})}
	h.clients[c] = struct{}{}
	h.m.WSClients.Set(float64(len(h.clients)))
	return c
}

// Unsubscribe removes a client that is going away on its own. Safe to call more than once.
func (h *Hub) Unsubscribe(c *Client) {
	h.mu.Lock()
	h.remove(c, "unsubscribed")
	h.mu.Unlock()
}

// remove must be called with h.mu held for writing.
func (h *Hub) remove(c *Client, reason string) {
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	c.once.Do(func() {
		c.reason = reason
		close(c.done)
	})
	h.m.WSClients.Set(float64(len(h.clients)))
}

// Broadcast offers payload to every client subscribed to deviceID (or to all devices).
// payload is shared between clients and must not be modified afterwards.
func (h *Hub) Broadcast(deviceID string, payload []byte) {
	var slow []*Client
	h.mu.RLock()
	for c := range h.clients {
		if c.device != "" && c.device != deviceID {
			continue
		}
		select {
		case c.send <- payload:
		default:
			slow = append(slow, c)
		}
	}
	h.mu.RUnlock()

	if len(slow) == 0 {
		return
	}
	h.mu.Lock()
	for _, c := range slow {
		if _, still := h.clients[c]; still {
			h.remove(c, "slow_consumer")
			h.m.WSSlowDisconnects.Inc()
		}
	}
	h.mu.Unlock()
}

// Len returns the number of connected clients.
func (h *Hub) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// CloseAll disconnects every client and refuses new subscriptions (shutdown).
func (h *Hub) CloseAll() {
	h.mu.Lock()
	h.closed = true
	for c := range h.clients {
		h.remove(c, "shutdown")
	}
	h.mu.Unlock()
}
