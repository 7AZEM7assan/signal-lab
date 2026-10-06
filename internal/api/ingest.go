package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"signallab/internal/event"
	"signallab/internal/metrics"
	"signallab/internal/pipeline"
)

const maxReportedRejections = 50

type ingestRequest struct {
	Events []json.RawMessage `json:"events"`
}

type rejection struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

type queueState struct {
	Depth    int `json:"depth"`
	Capacity int `json:"capacity"`
}

type ingestResponse struct {
	BatchID             string         `json:"batch_id"`
	Received            int            `json:"received"`
	Accepted            int            `json:"accepted"`
	Rejected            int            `json:"rejected"`
	Rejections          []rejection    `json:"rejections,omitempty"`
	RejectionCounts     map[string]int `json:"rejection_counts,omitempty"`
	RejectionsTruncated bool           `json:"rejections_truncated,omitempty"`
	Queue               queueState     `json:"queue"`
	Error               *apiError      `json:"error,omitempty"`
}

// handleIngest validates a batch and enqueues its valid records.
//
// Acknowledgement contract: 202 means every valid record was placed on the
// in-memory queue (not that it is stored). Invalid records are reported per
// index and are not retryable. If the valid records do not fit in the queue,
// NONE are accepted and the response is 429 with Retry-After; resend the whole
// batch. The batch is atomic with respect to the queue.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	batchID := RequestID(r.Context())
	now := s.Now()

	r.Body = http.MaxBytesReader(w, r.Body, s.Cfg.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	var req ingestRequest
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds the configured limit")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", `body must be a JSON object like {"events":[...]}`)
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "unexpected data after the JSON object")
		return
	}
	switch {
	case len(req.Events) == 0:
		writeError(w, http.StatusBadRequest, "empty_batch", "events must contain at least one record")
		return
	case len(req.Events) > s.Cfg.MaxBatchEvents:
		writeError(w, http.StatusRequestEntityTooLarge, "batch_too_large", "too many events in one request")
		return
	}

	resp := ingestResponse{BatchID: batchID, Received: len(req.Events), RejectionCounts: map[string]int{}}
	valid := make([]event.Event, 0, len(req.Events))
	dedupe := event.NewBatchDeduper()
	for i, raw := range req.Events {
		ev, rej := event.Parse(raw, s.limits, now)
		if rej == nil {
			rej = dedupe.Check(ev)
		}
		if rej != nil {
			resp.Rejected++
			resp.RejectionCounts[rej.Reason]++
			s.Metrics.ValidationFailures.WithLabelValues(rej.Reason).Inc()
			if len(resp.Rejections) < maxReportedRejections {
				resp.Rejections = append(resp.Rejections, rejection{Index: i, Reason: rej.Reason, Detail: rej.Detail})
			} else {
				resp.RejectionsTruncated = true
			}
			continue
		}
		valid = append(valid, ev)
	}
	if resp.Rejected == 0 {
		resp.RejectionCounts = nil
	}
	s.Metrics.IngestEvents.WithLabelValues(metrics.IngestRejectedInvalid).Add(float64(resp.Rejected))

	status := http.StatusAccepted
	var enqueueErr error
	if len(valid) > 0 {
		enqueueErr = s.Ingest.Enqueue(batchID, valid)
	}
	switch err := enqueueErr; {
	case err == nil:
		resp.Accepted = len(valid)
		s.Metrics.IngestEvents.WithLabelValues(metrics.IngestAccepted).Add(float64(len(valid)))
	case errors.Is(err, pipeline.ErrQueueFull):
		status = http.StatusTooManyRequests
		resp.Error = &apiError{"queue_full", "ingest queue is full; no events from this batch were accepted; retry the batch after Retry-After"}
		s.Metrics.IngestEvents.WithLabelValues(metrics.IngestRejectedOverload).Add(float64(len(valid)))
		w.Header().Set("Retry-After", "1")
	case errors.Is(err, pipeline.ErrClosed):
		status = http.StatusServiceUnavailable
		resp.Error = &apiError{"shutting_down", "service is draining; retry against another instance or later"}
		s.Metrics.IngestEvents.WithLabelValues(metrics.IngestRejectedShutdown).Add(float64(len(valid)))
		w.Header().Set("Retry-After", "5")
	default:
		s.Log.Error("enqueue failed", "batch_id", batchID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	resp.Queue = queueState{Depth: s.Ingest.Depth(), Capacity: s.Ingest.Capacity()}
	s.Log.Debug("batch processed", "batch_id", batchID, "received", resp.Received, "accepted", resp.Accepted, "rejected", resp.Rejected, "status", status)
	writeJSON(w, status, resp)
}
