package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"signallab/internal/event"
)

const pingInterval = 30 * time.Second

// handleWS streams live "event" and "alert" messages. Optional ?device_id= filters
// by device. The first frame is a "hello" sent after the subscription is
// registered, so a client that waits for it cannot miss later messages.
//
// Messages are live-only; there is no replay. A client that cannot keep up is
// disconnected (close code 1008) and should resync through the HTTP query API.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device_id")
	if device != "" && !event.ValidID(device) {
		writeError(w, http.StatusBadRequest, "invalid_parameter", "device_id is not a valid identifier")
		return
	}
	client := s.Hub.Subscribe(device)
	if client == nil {
		writeError(w, http.StatusServiceUnavailable, "too_many_clients", "websocket client limit reached or service shutting down")
		return
	}
	defer s.Hub.Unsubscribe(client)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.Cfg.WSAllowedOrigins})
	if err != nil {
		return // Accept has already written the HTTP error (e.g. bad Origin)
	}
	defer conn.CloseNow()

	// The client is not expected to send data; CloseRead services control frames
	// and cancels ctx when the peer goes away.
	ctx := conn.CloseRead(r.Context())

	hello, _ := json.Marshal(map[string]any{"type": "hello", "data": map[string]any{
		"server_time": s.Now().UTC(), "queue_capacity": s.Ingest.Capacity(),
	}})
	if err := s.wsWrite(ctx, conn, hello); err != nil {
		return
	}

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case msg := <-client.Messages():
			if err := s.wsWrite(ctx, conn, msg); err != nil {
				if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
					// The peer stopped reading at the TCP level: same policy as a full buffer.
					s.Metrics.WSSlowDisconnects.Inc()
					s.Log.Warn("websocket write timed out; disconnecting slow client")
				}
				return
			}
		case <-client.Done():
			if client.Reason() == "slow_consumer" {
				_ = conn.Close(websocket.StatusPolicyViolation, "slow consumer")
			} else {
				_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
			}
			return
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, s.Cfg.WSWriteTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// wsWrite writes one text frame under the write timeout, so a peer that stops
// reading at the TCP level cannot hold this goroutine forever.
func (s *Server) wsWrite(ctx context.Context, conn *websocket.Conn, msg []byte) error {
	wctx, cancel := context.WithTimeout(ctx, s.Cfg.WSWriteTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, msg)
}
