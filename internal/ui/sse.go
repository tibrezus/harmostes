package ui

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// streamFragments — the shared SSE engine for server-rendered live fragments
// (the wall grid, the run-detail graph). One mechanism: initial paint, then a
// re-render per coalesced event burst, heartbeats for proxies, and an optional
// slow ticker for time-dependent content. Exactly one goroutine writes to w.
// ---------------------------------------------------------------------------

// streamFragments renders `eventName` fragments over SSE until the client
// disconnects. `cancel` deregisters `sub` from the hub; `render` produces the
// fragment HTML; `tickerInterval` (>0) forces periodic re-renders for
// time-dependent content (e.g. relative ages).
func (s *Server) streamFragments(w http.ResponseWriter, r *http.Request, sub *subscriber, cancel func(), eventName string, render func() (string, error), tickerInterval time.Duration) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeAPIError(w, http.StatusInternalServerError, "streaming not supported")
		cancel()
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	defer cancel()

	send := func() {
		html, err := render()
		if err != nil {
			s.logger.Error("sse render", "event", eventName, "err", err)
			return // keep the stream alive; the next event retries
		}
		fmt.Fprintf(w, "event: %s\n", eventName)
		for _, line := range strings.Split(html, "\n") {
			fmt.Fprintf(w, "data: %s\n", line)
		}
		fmt.Fprint(w, "\n")
		flusher.Flush()
	}

	send() // initial paint — correct before any event arrives

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	var tickerC <-chan time.Time // nil = disabled (receive on nil blocks)
	if tickerInterval > 0 {
		ticker := time.NewTicker(tickerInterval)
		defer ticker.Stop()
		tickerC = ticker.C
	}

	// Event bursts coalesce: the timer signals the select loop via a channel
	// (the timer's goroutine never writes to w).
	kicks := make(chan struct{}, 1)
	var debounce *time.Timer
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-tickerC:
			send()
		case <-kicks:
			send()
		case _, ok := <-sub.ch:
			if !ok {
				return
			}
			if debounce == nil {
				debounce = time.AfterFunc(wallDebounce, func() {
					select {
					case kicks <- struct{}{}:
					default:
					}
				})
			} else {
				debounce.Reset(wallDebounce)
			}
		}
	}
}
