package agenttransport

import (
	"net/http"
	"time"

	"docker-manager/internal/agentwire"
)

// The WebSocket library owns upgrade validation, but its HTTP diagnostics may
// contain request headers. Keep those headers and bodies private until it has
// selected 101; all earlier failures use the Manager's fixed error contract.
type upgradeResponse struct {
	http.ResponseWriter
	header    http.Header
	committed bool
	upgraded  bool
}

func (w *upgradeResponse) Header() http.Header { return w.header }

// Preserve the underlying writer's actual optional capabilities. Declaring
// Hijacker here would incorrectly make a non-Hijacker appear upgradeable.
func (w *upgradeResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *upgradeResponse) WriteHeader(status int) {
	if w.committed {
		return
	}
	w.committed = true
	if status == http.StatusSwitchingProtocols {
		w.upgraded = true
		header := w.ResponseWriter.Header()
		for key := range header {
			delete(header, key)
		}
		for key, values := range w.header {
			header[key] = append([]string(nil), values...)
		}
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if status >= 500 {
		reject(w.ResponseWriter, "internal_error", http.StatusInternalServerError)
	} else {
		reject(w.ResponseWriter, "invalid_field", http.StatusBadRequest)
	}
}

func (w *upgradeResponse) Write(raw []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusBadRequest)
	}
	// WebSocket frames go through the hijacked socket, never this method. This
	// also suppresses the library's fallback http.Error after a failed Hijack.
	return len(raw), nil
}

func acceptAgentUpgrade(w http.ResponseWriter, r *http.Request) (*agentwire.Handshake, error) {
	response := &upgradeResponse{ResponseWriter: w, header: w.Header().Clone()}
	handshake, err := agentwire.Accept(response, r)
	if err != nil {
		if !response.committed {
			response.WriteHeader(http.StatusInternalServerError)
		} else if response.upgraded {
			// A failed Hijack leaves HTTP owning the connection. Its 101 is
			// already committed, so end I/O instead of writing another response.
			controller := http.NewResponseController(w)
			now := time.Now()
			_ = controller.SetReadDeadline(now)
			_ = controller.SetWriteDeadline(now)
		}
	}
	return handshake, err
}
