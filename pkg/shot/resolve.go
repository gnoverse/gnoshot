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
	fmt.Fprintf(&sb, `];
	  const PAD = %d;
	  for (const [sel, matched] of chain) {
	    const e = document.querySelector(sel);
	    if (!e) continue;
	    let r = e.getBoundingClientRect();
	    if (r.width <= 0 || r.height <= 0) continue;
	    // Frame the render before photographing it.
	    //
	    // The crop is the element's own box, so without this the realm's first
	    // heading is glued to the pixel at 0,0 and the last line runs off the
	    // bottom edge. Padding the element grows that box, so the breathing
	    // room lands inside the crop rather than being cropped away.
	    //
	    // Padding only, never a width. Widening the element to fill the frame
	    // was the obvious next step and it is wrong: gnoweb lays the realm view
	    // beside an "On this page" sidebar, so a wider box does not gain empty
	    // space, it grows over the neighbour and photographs half a table of
	    // contents down the right edge. Verified 2026-09-23 on /r/gov/dao.
	    e.style.boxSizing = 'content-box';
	    e.style.padding = PAD + 'px';
	    // The page paints on its own background, and the element usually has
	    // none: without this the new padding is transparent and the crop shows
	    // whatever is behind it.
	    const bg = getComputedStyle(document.body).backgroundColor;
	    if (bg && bg !== 'rgba(0, 0, 0, 0)') e.style.backgroundColor = bg;
	    r = e.getBoundingClientRect();
	    return {
	      matched, selector: sel,
	      x: r.x + window.scrollX, y: r.y + window.scrollY,
	      w: r.width, h: r.height,
	      doc: Math.max(document.body.scrollHeight, document.documentElement.scrollHeight),
	    };
	  }`, FramePadding)
	sb.WriteString(`
	  return {
	    matched: "status", selector: "",
	    x: 0, y: 0, w: 0, h: 0,
	    doc: Math.max(document.body.scrollHeight, document.documentElement.scrollHeight),
	  };
	})()`)
	return sb.String()
}()

// docHeightJS is what runs instead of the selector chain on a site host.
//
// The chain is gnoweb's own class names and it also *mutates* what it matches,
// padding the element so the crop has breathing room. Neither is right on
// somebody else's page: the class names mean nothing there, and rewriting a
// third-party document before photographing it would be photographing
// something that never existed. All a whole-page capture needs is how tall the
// document is.
const docHeightJS = `(() => ({matched: "site", selector: "", x: 0, y: 0, w: 0, h: 0,
  doc: Math.max(document.body ? document.body.scrollHeight : 0,
                document.documentElement.scrollHeight)}))()`

// FramePadding is the breathing room added around a render before it is
// photographed, in CSS pixels.
//
// The crop is the matched element's own bounding box, so anything not inside
// that box is not in the picture. Padding the element is what puts margin in
// the frame rather than around it.
const FramePadding = 28

// RenderVersion changes whenever the capture recipe does.
//
// The cache key is a hash of the gnoweb response, which answers "has the page
// changed" and not "would we photograph it differently today". Without this, a
// change to the framing leaves every already-captured realm showing the old
// one, because the page did not move. Bump it when the recipe moves.
const RenderVersion = 2

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
	// The recipe is part of the freshness, so changing how a page is framed
	// invalidates every capture taken the old way.
	h := sha256.New()
	fmt.Fprintf(h, "v%d\n", RenderVersion)
	h.Write(body)
	sum := h.Sum(nil)
	p := &Probe{Status: resp.StatusCode, Freshness: hex.EncodeToString(sum), Body: body}
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
	sites map[string]bool
}

// NewAllowlist builds an allowlist from two comma-separated host lists.
//
// The split is not cosmetic. A gnoweb host is photographed through the selector
// chain, which is a set of gnoweb's own class names; a site host has no such
// structure and is photographed whole. Running the chain against a host it was
// never written for is how every third-party app came out classified as an
// error page. Keeping the two lists apart means the operator states which kind
// of page a host serves, rather than the service guessing from what it found.
func NewAllowlist(spec, siteSpec string) *Allowlist {
	a := &Allowlist{hosts: map[string]bool{}, sites: map[string]bool{}}
	fill := func(dst map[string]bool, spec string) {
		for _, h := range strings.Split(spec, ",") {
			h = strings.ToLower(strings.TrimSpace(h))
			if h != "" {
				dst[h] = true
			}
		}
	}
	fill(a.hosts, spec)
	fill(a.sites, siteSpec)
	// A host named in both is a configuration mistake with a quiet failure
	// mode, so gnoweb wins and the site entry is dropped: the selector chain
	// produces a better picture wherever it applies.
	for h := range a.hosts {
		delete(a.sites, h)
	}
	return a
}

// IsSite reports whether a URL is on a host photographed whole.
func (a *Allowlist) IsSite(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return a.sites[strings.ToLower(u.Hostname())]
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
	h := strings.ToLower(u.Hostname())
	if !a.hosts[h] && !a.sites[h] {
		return "", fmt.Errorf("host %q not on the allowlist", u.Hostname())
	}
	u.Fragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}
