// Package shot captures pictures of gno.land pages: the whole gnoweb page, and
// the crop that actually matters, the realm's own rendered output.
//
// The design this implements, and every measurement behind it, is specified
// elsewhere; the load-bearing decisions are repeated here as comments where the
// code would otherwise look arbitrary.
package shot

import (
	"fmt"
	"strings"
	"time"
)

// Mode is what part of the page a capture covers.
type Mode string

const (
	// ModeRender is the realm's own output, resolved through the selector
	// chain. This is the capture worth showing.
	ModeRender Mode = "render"
	// ModePage is the whole gnoweb page, chrome included.
	ModePage Mode = "page"
)

// ParseMode accepts the query-string spelling and defaults to render.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "", string(ModeRender):
		return ModeRender, nil
	case string(ModePage):
		return ModePage, nil
	}
	return "", fmt.Errorf("unknown mode %q", s)
}

// Matched names which branch of the selector chain produced the crop. It is the
// load-bearing field of the manifest: a consumer decides from it whether it is
// holding a picture of a working realm or a picture of an error page.
type Matched string

const (
	// MatchedRealm is a realm render: md-renderer.c-realm-view.
	MatchedRealm Matched = "realm"
	// MatchedReadme is a documented directory or pure package, still a good
	// crop: md-renderer.c-readme-view.
	MatchedReadme Matched = "readme"
	// MatchedDirectory is an undocumented directory listing:
	// article.b-directory.
	MatchedDirectory Matched = "directory"
	// MatchedStatus is an error, a 404, or a package submitted and not yet
	// enabled. Never present one of these as a picture of a working realm.
	MatchedStatus Matched = "status"
)

// Status refines MatchedStatus. All three render the same full-screen box and
// are visually identical, so they are told apart by the response body rather
// than by the status code: a package that has been submitted and is not yet
// enabled answers 404, exactly like one that does not exist.
type Status string

const (
	StatusOK       Status = "ok"
	StatusNotFound Status = "not_found"
	// StatusInert is a package that has been submitted and is waiting to be
	// enabled. It is the one negative state that flips to a real render with
	// no request from us, which is why it gets a short negative TTL.
	StatusInert Status = "inert"
	StatusError Status = "error"
)

// NegativeTTL is how long a non-capture may be trusted.
func (s Status) NegativeTTL() time.Duration {
	switch s {
	case StatusNotFound:
		return 24 * time.Hour
	case StatusInert:
		return 5 * time.Minute
	case StatusError:
		return 60 * time.Second
	}
	return 0
}

// Rung is one step of the derivative ladder. Capture happens once at DSF 2 and
// every rung is derived from that master; re-rendering per size costs a browser
// round trip to produce pixels a resize already has.
type Rung struct {
	Name    string
	W, H    int
	Quality int // WebP quality for the lossy derivative
}

// Rungs is the whole ladder, in the order a sweep generates them.
//
// Three rungs, not four. `avatar` 96x96 was specified and then cut: a square
// cover crop from the top of a render 816 CSS px wide is an 8.5x downscale, so
// 16 px body text lands under 2 px tall. That is not a picture of a realm, it
// is an illegible fragment of its first heading; an identicon is the honest
// answer for a square identity mark.
var Rungs = []Rung{
	{Name: "og", W: 1200, H: 630, Quality: 80},
	{Name: "hero@1x", W: 640, H: 360, Quality: 80},
	{Name: "hero@2x", W: 1280, H: 720, Quality: 80},
	{Name: "thumb@1x", W: 160, H: 90, Quality: 80},
	{Name: "thumb@2x", W: 320, H: 180, Quality: 80},
}

// RungByName resolves a size parameter, accepting both the rung name and the
// bare family name with a dpr.
func RungByName(size string, dpr int) (Rung, bool) {
	if size == "" {
		size = "og"
	}
	if dpr != 1 && dpr != 2 {
		dpr = 2
	}
	want := size
	if !strings.Contains(size, "@") && size != "og" {
		want = fmt.Sprintf("%s@%dx", size, dpr)
	}
	for _, r := range Rungs {
		if r.Name == want {
			return r, true
		}
	}
	return Rung{}, false
}

// Theme is which gnoweb palette to capture.
//
// Light, and only light, for v1: gnoweb with no theme cookie emits no
// data-theme and headless Chrome defaults to light, so light is what a
// first-time visitor sees and what a social card should look like. The
// parameter exists and the worker honours it; only the sweep is light-only.
type Theme string

const (
	ThemeLight Theme = "light"
	ThemeDark  Theme = "dark"
)

func ParseTheme(s string) (Theme, error) {
	switch s {
	case "", string(ThemeLight):
		return ThemeLight, nil
	case string(ThemeDark):
		return ThemeDark, nil
	}
	return "", fmt.Errorf("unknown theme %q", s)
}

// Box is a bounding box in CSS pixels.
type Box struct {
	X, Y, W, H float64
}

// Entry is one manifest row: everything known about one gnoweb URL.
type Entry struct {
	URL      string  `json:"url"`
	Theme    Theme   `json:"theme"`
	Matched  Matched `json:"matched"`
	Selector string  `json:"selector,omitempty"`
	Box      struct {
		W int `json:"w"`
		H int `json:"h"`
	} `json:"box"`
	Truncated      bool   `json:"truncated"`
	UpstreamStatus int    `json:"upstream_status"`
	State          Status `json:"state"`
	// Freshness is sha256 of the gnoweb response body. A realm's render is a
	// function of chain state and changes with no URL change, so the URL is
	// not a cache key and a TTL is a guess; the body is exact, and it covers
	// the :path arguments and the gnoweb build id for free.
	Freshness  string            `json:"freshness"`
	CapturedAt time.Time         `json:"captured_at"`
	ProbedAt   time.Time         `json:"probed_at"`
	Object     string            `json:"object,omitempty"`
	Rungs      map[string]int    `json:"rungs,omitempty"` // rung name -> bytes
	Modes      map[string]string `json:"modes,omitempty"` // mode -> master file
}

// HasImage reports whether a real capture exists, as opposed to a recorded
// negative state.
func (e *Entry) HasImage() bool { return e != nil && e.Object != "" && e.Matched != MatchedStatus }
