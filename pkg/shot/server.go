package shot

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

// Server is the HTTP surface.
type Server struct {
	svc         *Service
	adminToken  string
	maxBodyEtag bool
}

// NewServer wires the handlers. adminToken may be empty, in which case
// /invalidate is refused rather than left open.
func NewServer(svc *Service, adminToken string) *Server {
	return &Server{svc: svc, adminToken: adminToken}
}

// Routes registers every handler on a mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /shot", s.handleShot)
	mux.HandleFunc("GET /meta", s.handleMeta)
	mux.HandleFunc("GET /tile", s.handleTile)
	mux.HandleFunc("POST /invalidate", s.handleInvalidate)
	mux.HandleFunc("GET /healthz", s.handleHealth)
}

func (s *Server) handleShot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pageURL, err := s.svc.Allow().Check(q.Get("url"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mode, err := ParseMode(q.Get("mode"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	theme, err := ParseTheme(q.Get("theme"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dpr, _ := strconv.Atoi(q.Get("dpr"))
	rung, ok := RungByName(q.Get("size"), dpr)
	if !ok {
		http.Error(w, "unknown size", http.StatusBadRequest)
		return
	}

	body, entry, status := s.svc.Serve(pageURL, theme, mode, rung)
	h := w.Header()
	h.Set("Content-Type", "image/webp")
	h.Set("Vary", "Accept")
	if entry != nil {
		h.Set("X-Gnoshot-Matched", string(entry.Matched))
		h.Set("X-Gnoshot-State", string(entry.State))
		if entry.Truncated {
			h.Set("X-Gnoshot-Truncated", "1")
		}
	}
	switch status {
	case http.StatusAccepted:
		// Queued. Never cache a placeholder: the real picture is minutes away
		// and a cached tile would outlive it in every intermediary.
		h.Set("Cache-Control", "no-store")
	default:
		// Long-lived but revalidating, never `immutable`.
		//
		// `immutable` would be right if v= covered everything that decides the
		// bytes. It does not: a caller pins it to something about the *page*,
		// typically a block height, and the picture also depends on the recipe
		// here. Change how a render is framed and every returning visitor keeps
		// the old one until their v= moves, which for a genesis package nobody
		// has ever called is never.
		//
		// stale-while-revalidate keeps the paint instant anyway: the cached
		// image is served immediately and the conditional request happens
		// behind it, answered by the ETag with 304 and no body in the normal
		// case.
		if r.URL.Query().Get("v") != "" {
			h.Set("Cache-Control", "public, max-age=86400, stale-while-revalidate=604800")
		} else {
			h.Set("Cache-Control", "public, max-age=300")
		}
		if entry != nil && entry.Freshness != "" {
			// Bounded, not `[:16]`. A freshness shorter than that is not
			// something the probe produces, and slicing it took the whole
			// handler down with a panic rather than serving a slightly odd
			// ETag: a stored value from an older build or a hand-written row
			// should not be able to crash a request.
			h.Set("ETag", `"`+shortHash(entry.Freshness)+`-`+rung.Name+`"`)
			if match := r.Header.Get("If-None-Match"); match != "" && match == h.Get("ETag") {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	// 404 is never the answer here. A gno.land URL that does not resolve still
	// gets an image, because the consumer is an <img> and its only alternative
	// is a broken-image glyph.
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Printf("write shot: %v", err)
	}
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	pageURL, err := s.svc.Allow().Check(r.URL.Query().Get("url"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	theme, err := ParseTheme(r.URL.Query().Get("theme"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e, err := s.svc.Lookup(pageURL, theme)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if e == nil {
		s.svc.Enqueue(pageURL, theme, true)
		writeJSON(w, http.StatusAccepted, map[string]any{"url": pageURL, "matched": "", "queued": true})
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// handleTile serves a generated placeholder without consulting the manifest.
//
// It exists so a consumer that already knows the state from its own data can
// draw the right tile without a round trip through a capture that is never
// going to happen.
func (s *Server) handleTile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := q.Get("path")
	if path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	kind := TileKind(q.Get("kind"))
	if _, ok := tileCopy[kind]; !ok {
		kind = TileNone
	}
	dpr, _ := strconv.Atoi(q.Get("dpr"))
	rung, ok := RungByName(q.Get("size"), dpr)
	if !ok {
		http.Error(w, "unknown size", http.StatusBadRequest)
		return
	}
	b, err := EncodeTile(path, kind, rung)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/webp")
	// Deterministic from (path, kind, rung) and therefore immutable.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

func (s *Server) handleInvalidate(w http.ResponseWriter, r *http.Request) {
	if s.adminToken == "" || r.Header.Get("Authorization") != "Bearer "+s.adminToken {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		URL   string `json:"url"`
		Theme Theme  `json:"theme"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pageURL, err := s.svc.Allow().Check(req.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	theme, err := ParseTheme(string(req.Theme))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ok := s.svc.Enqueue(pageURL, theme, true)
	writeJSON(w, http.StatusOK, map[string]any{"queued": ok})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.Store().Stats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.svc.mu.Lock()
	inflight := len(s.svc.inFlight)
	s.svc.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime":       time.Since(s.svc.started).Round(time.Second).String(),
		"workers":      s.svc.cfg.Workers,
		"queue":        map[string]int{"interactive": len(s.svc.interactive), "background": len(s.svc.background)},
		"in_flight":    inflight,
		"entries":      st.Entries,
		"captured":     st.Captured,
		"oldest_probe": st.Oldest,
		"chrome":       ChromePath(),
		// So an operator can tell at a glance whether a box is serving pictures
		// taken with the current recipe, without diffing images by eye.
		"render_version": RenderVersion,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// shortHash trims a digest for use in an ETag, without assuming its length.
func shortHash(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
