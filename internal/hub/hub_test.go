package hub

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"signallab/internal/metrics"
)

func recv(t *testing.T, c *Client) string {
	t.Helper()
	select {
	case m := <-c.Messages():
		return string(m)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
		return ""
	}
}

func TestBroadcastReachesAllClients(t *testing.T) {
	h := New(4, 10, metrics.New())
	a, b := h.Subscribe(""), h.Subscribe("")
	h.Broadcast("press-01", []byte("one"))
	if recv(t, a) != "one" || recv(t, b) != "one" {
		t.Fatal("both clients should receive the message")
	}
}

func TestDeviceFilter(t *testing.T) {
	h := New(4, 10, metrics.New())
	all, only := h.Subscribe(""), h.Subscribe("pump-02")
	h.Broadcast("press-01", []byte("x"))
	h.Broadcast("pump-02", []byte("y"))
	if recv(t, all) != "x" || recv(t, all) != "y" {
		t.Fatal("unfiltered client should get both")
	}
	if got := recv(t, only); got != "y" {
		t.Fatalf("filtered client got %q, want only pump-02's message", got)
	}
}

func TestSlowClientIsDisconnectedWithoutAffectingOthers(t *testing.T) {
	m := metrics.New()
	h := New(3, 10, m)
	slow, fast := h.Subscribe(""), h.Subscribe("")

	for i := 0; i < 3; i++ { // fills slow's buffer; fast keeps up
		h.Broadcast("d", []byte{byte('a' + i)})
		recv(t, fast)
	}
	h.Broadcast("d", []byte("overflow")) // slow's buffer is full: it must be dropped, not block

	select {
	case <-slow.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("slow client should have been disconnected")
	}
	if slow.Reason() != "slow_consumer" {
		t.Fatalf("reason = %q", slow.Reason())
	}
	if got := recv(t, fast); got != "overflow" {
		t.Fatalf("fast client got %q", got)
	}
	if h.Len() != 1 {
		t.Fatalf("hub should hold 1 client, has %d", h.Len())
	}
	if got := testutil.ToFloat64(m.WSSlowDisconnects); got != 1 {
		t.Fatalf("slow disconnect metric = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.WSClients); got != 1 {
		t.Fatalf("ws clients gauge = %v, want 1", got)
	}
}

func TestBroadcastNeverBlocks(t *testing.T) {
	h := New(1, 10, metrics.New())
	h.Subscribe("") // never read
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Broadcast("d", []byte("m"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast blocked on a client that is not reading")
	}
}

func TestMaxClients(t *testing.T) {
	h := New(1, 2, metrics.New())
	if h.Subscribe("") == nil || h.Subscribe("") == nil {
		t.Fatal("first two subscriptions should succeed")
	}
	if h.Subscribe("") != nil {
		t.Fatal("third subscription should be refused")
	}
}

func TestUnsubscribeIsIdempotentAndFreesSlot(t *testing.T) {
	h := New(1, 1, metrics.New())
	c := h.Subscribe("")
	h.Unsubscribe(c)
	h.Unsubscribe(c)
	if h.Len() != 0 || h.Subscribe("") == nil {
		t.Fatal("slot should be free after unsubscribe")
	}
}

func TestCloseAll(t *testing.T) {
	h := New(1, 5, metrics.New())
	a, b := h.Subscribe(""), h.Subscribe("")
	h.CloseAll()
	for _, c := range []*Client{a, b} {
		select {
		case <-c.Done():
		default:
			t.Fatal("client should be closed")
		}
		if c.Reason() != "shutdown" {
			t.Fatalf("reason = %q", c.Reason())
		}
	}
	if h.Subscribe("") != nil {
		t.Fatal("no subscriptions after CloseAll")
	}
}
