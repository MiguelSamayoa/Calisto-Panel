package hub

import (
	"fmt"
	"net/http"
	"time"
)

type subscriber struct {
	ch     chan []byte
	closed bool
}

func frame(event string, data []byte) []byte {
	return fmt.Appendf(nil, "event: %s\ndata: %s\n\n", event, data)
}

// publishLocked fans a frame out to every subscriber. A subscriber whose buffer is
// full is dropped; its EventSource reconnects and receives a fresh snapshot.
func (h *Hub) publishLocked(event string, data []byte) {
	f := frame(event, data)
	for s := range h.subs {
		select {
		case s.ch <- f:
		default:
			h.dropLocked(s)
		}
	}
}

func (h *Hub) dropLocked(s *subscriber) {
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	delete(h.subs, s)
}

// subscribe registers a subscriber pre-loaded with hello and a snapshot of every
// domain, so a new client does not wait for the next poll. Doing it under the lock
// guarantees no event is missed or duplicated between the snapshot and the live feed.
func (h *Hub) subscribe() *subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &subscriber{ch: make(chan []byte, subBuffer)}
	s.ch <- frame("hello", h.hello)
	s.ch <- frame("sensors", h.sensorsPayloadLocked())
	s.ch <- frame("ci", h.ciPayloadLocked())
	if len(h.actions) > 0 {
		s.ch <- frame("actions", h.actionsPayloadLocked())
	}
	h.subs[s] = struct{}{}
	return s
}

func (h *Hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(s)
}

// ServeStream is the GET /api/stream handler. The http.Server must not set a
// WriteTimeout, or it would cut the stream.
func (h *Hub) ServeStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil || rc.Flush() != nil {
		return
	}

	s := h.subscribe()
	defer h.unsubscribe(s)

	idle := time.NewTimer(h.keepalive)
	defer idle.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case f, ok := <-s.ch:
			if !ok {
				return
			}
			if _, err := w.Write(f); err != nil || rc.Flush() != nil {
				return
			}
			idle.Reset(h.keepalive)
		case <-idle.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
			idle.Reset(h.keepalive)
		}
	}
}
