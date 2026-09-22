package shot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The selector chain, in resolution order. Take the first candidate whose
// bounding box has non-zero area, and record which one matched.
//
// The class must be pinned. A bare md-renderer also matches $source
// (md-renderer.c-overview-view) and $help (a classless md-renderer), neither of
// which is a realm render, and both of which are thousands of pixels tall.
//
// Verified against live gnoweb on gnoland-1, 2026-09-22:
//
//	/r/gov/dao          -> md-renderer.c-realm-view
//	/r/moul/config/v0   -> md-renderer.c-readme-view
//	/r/moul/config      -> article.b-directory
//	/p/moul/txlink      -> article.b-directory
//	/r/does/not/exist   -> 404, .c-full-screen, nothing in the chain
var selectorChain = []struct {
	Sel     string
	Matched Matched
}{
	{"md-renderer.c-realm-view", MatchedRealm},
	{"md-renderer.c-readme-view", MatchedReadme},
	{"article.b-directory", MatchedDirectory},
}

// resolveJS walks the chain in the page and returns the first non-zero-area
// match.
//
// Non-zero area, not a null check: md-renderer.c-realm-view exists on /u/<user>
// with width 0 and height 0. Asking Chrome to photograph a zero-area node does
// not fail, it never returns, and one user profile in the queue holds a worker
// for the whole deadline.
var resolveJS = func() string {
	var sb strings.Builder
	sb.WriteString("(() => { const chain = [")
	for i, c := range selectorChain {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "[%q,%q]", c.Sel, string(c.Matched))
	}
	sb.WriteString(`];
	  for (const [sel, matched] of chain) {
	    const e = document.querySelector(sel);
	    if (!e) continue;
	    const r = e.getBoundingClientRect();
	    if (r.width <= 0 || r.height <= 0) continue;
	    return {
	      matched, selector: sel,
	      x: r.x + window.scrollX, y: r.y + window.scrollY,
	      w: r.width, h: r.height,
	      doc: Math.max(document.body.scrollHeight, document.documentElement.scrollHeight),
	    };
	  }
	  return {
	    matched: "status", selector: "",
	    x: 0, y: 0, w: 0, h: 0,
	    doc: Math.max(document.body.scrollHeight, document.documentElement.scrollHeight),
	  };
	})()`)
	return sb.String()
}()

// Probe is the cheap half of a capture: one HTTP GET that answers both "has
// anything changed" and "is this even a page worth photographing".
//
// Measured against vm/qrender, which is the obvious alternative: the HTTP GET
// is 45 ms where qrender is 410 to 610 ms, and it covers the gnoweb build id
// and the :path arguments, which qrender does not see at all.
type Probe struct {
	Status    int
	Freshness string
	State     Status
	Body      []byte
}

// inertMarker is the body text that separates "submitted, not yet enabled"
// from "does not exist". Both answer 404 and both render the same full-screen
// box, so the status code cannot tell them apart.
//
// Verified 2026-09-22 on gno.land/r/g1r6lutt.../pixelgnomes, which answers 404
// with "Not Yet Enabled / This package has been submitted. Its source is not
// readable and it cannot be called until it is enabled."
const inertMarker = "This package has been submitted."

// DoProbe fetches the page and classifies it without launching a browser.
func DoProbe(ctx context.Context, client *http.Client, pageURL string) (*Probe, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	p := &Probe{Status: resp.StatusCode, Freshness: hex.EncodeToString(sum[:]), Body: body}
	switch {
	case resp.StatusCode == http.StatusOK:
		p.State = StatusOK
	case resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), inertMarker):
		p.State = StatusInert
	case resp.StatusCode == http.StatusNotFound:
		p.State = StatusNotFound
	default:
		p.State = StatusError
	}
	return p, nil
}

// UserAgent identifies the capture worker to gnoweb. Naming it is the courtesy
// that lets an operator tell our traffic from a scraper's in their logs.
const UserAgent = "gnoshot/1 (+https://github.com/gnoverse/gnoshot)"

// DefaultProbeClient is a client sized for the probe: short timeouts, no
// redirects followed silently into another host.
func DefaultProbeClient() *http.Client {
	return &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}

// Allowlist is the set of hosts a capture may be asked for.
//
// Not optional and not a nicety: an unbounded ?url= is an open proxy and a way
// to spend somebody else's CPU on headless Chrome. Everything outside the list
// is a 400 before any work happens.
type Allowlist struct {
	hosts map[string]bool
}

// NewAllowlist builds an allowlist from a comma-separated host list.
func NewAllowlist(spec string) *Allowlist {
	a := &Allowlist{hosts: map[string]bool{}}
	for _, h := range strings.Split(spec, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			a.hosts[h] = true
		}
	}
	return a
}

// DefaultAllowHosts is the production set: mainnet and the two public networks
// that currently serve a gnoweb.
//
// Verified 2026-09-22, all three answer 200 on /r/gov/dao. Retired stones are
// not listed: test6.testnets.gno.land no longer resolves, and an allowlist
// entry for a host that does not exist is a claim nobody will re-check.
const DefaultAllowHosts = "gno.land,staging.gno.land,pearl.testnets.gno.land"

// Check parses and validates a requested URL, returning it in normalised form.
func (a *Allowlist) Check(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("unparseable url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	if !a.hosts[strings.ToLower(u.Hostname())] {
		return "", fmt.Errorf("host %q not on the allowlist", u.Hostname())
	}
	u.Fragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}
