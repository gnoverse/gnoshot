package shot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// pathOf turns a gnoweb URL back into the package path a tile is labelled with.
func pathOf(pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil {
		return pageURL
	}
	p := strings.TrimPrefix(u.Path, "/")
	// $source, $help and the :path arguments are not part of the name.
	if i := strings.IndexAny(p, "$:"); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return u.Hostname()
	}
	return u.Hostname() + "/" + p
}

// PageURL builds the gnoweb URL for a package path.
func PageURL(base, pkgPath string) string {
	base = strings.TrimSuffix(base, "/")
	p := strings.TrimPrefix(pkgPath, "gno.land")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return base + p
}

// sweeper keeps the corpus warm.
//
// Two jobs: enumerate every known path so a first visitor to any realm finds a
// picture already there, and re-probe what has gone stale. Both run at
// background priority, behind anything a reader is waiting on.
func (s *Service) sweeper(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.SweepEvery)
	defer t.Stop()
	// One pass at boot, so a fresh deployment is warm within minutes rather
	// than on first traffic.
	s.sweepOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

func (s *Service) sweepOnce(ctx context.Context) {
	n := 0
	if s.cfg.SweepSource != "" {
		paths, err := FetchPaths(ctx, s.probe, s.cfg.SweepSource, s.cfg.SweepNetwork)
		if err != nil {
			log.Printf("sweep: enumerate: %v", err)
		}
		for _, p := range paths {
			u, err := s.allow.Check(PageURL(s.cfg.GnowebBase, p))
			if err != nil {
				continue
			}
			if s.Enqueue(u, ThemeLight, false) {
				n++
			}
		}
	}
	stale, err := s.store.Stale(s.cfg.RefreshAfter, 500)
	if err != nil {
		log.Printf("sweep: stale: %v", err)
	}
	for _, u := range stale {
		if s.Enqueue(u, ThemeLight, false) {
			n++
		}
	}
	log.Printf("sweep: queued %d", n)
}

// FetchPaths enumerates package paths from a mygnoscan-compatible API.
//
// The explorer already knows every path on every network it indexes, and it
// keeps that list current. Re-deriving it from the chain here would be a second
// copy of a fact somebody else owns.
func FetchPaths(ctx context.Context, client *http.Client, base, network string) ([]string, error) {
	var out []string
	for _, kind := range []string{"realms", "packages"} {
		u := fmt.Sprintf("%s/api/%s?network=%s&limit=2000", strings.TrimSuffix(base, "/"), kind, url.QueryEscape(network))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return out, err
		}
		req.Header.Set("User-Agent", UserAgent)
		resp, err := client.Do(req)
		if err != nil {
			return out, err
		}
		var payload struct {
			Items []struct {
				Path string `json:"path"`
			} `json:"items"`
			Realms []struct {
				Path string `json:"path"`
			} `json:"realms"`
			Packages []struct {
				Path string `json:"path"`
			} `json:"packages"`
		}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err != nil {
			return out, fmt.Errorf("decode %s: %w", kind, err)
		}
		for _, g := range [][]struct {
			Path string `json:"path"`
		}{payload.Items, payload.Realms, payload.Packages} {
			for _, it := range g {
				if it.Path != "" {
					out = append(out, it.Path)
				}
			}
		}
	}
	return out, nil
}
