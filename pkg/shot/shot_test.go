package shot

import (
	"context"
	"image"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAllowlistRefusesEverythingOffTheList(t *testing.T) {
	a := NewAllowlist(DefaultAllowHosts)
	tests := []struct {
		name string
		in   string
		want string // "" means it must be refused
	}{
		{"plain realm", "https://gno.land/r/gov/dao", "https://gno.land/r/gov/dao"},
		{"path args kept", "https://gno.land/r/gnoland/blog:p/hello", "https://gno.land/r/gnoland/blog:p/hello"},
		{"fragment dropped", "https://gno.land/r/gov/dao#x", "https://gno.land/r/gov/dao"},
		{"bare host gets a slash", "https://gno.land", "https://gno.land/"},
		{"other host", "https://evil.example/r/gov/dao", ""},
		{"lookalike host", "https://gno.land.evil.example/", ""},
		{"file scheme", "file:///etc/passwd", ""},
		{"javascript scheme", "javascript:alert(1)", ""},
		{"unparseable", "://", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.Check(tt.in)
			if tt.want == "" {
				if err == nil {
					t.Fatalf("Check(%q) = %q, want refusal", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("Check(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The probe is the only thing that separates a package waiting to be enabled
// from one that does not exist: both answer 404 and both render the same
// full-screen box, so the status code alone cannot do it.
func TestProbeTellsInertFromNotFound(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   Status
	}{
		{"live realm", 200, "<md-renderer class=\"c-realm-view\">hi</md-renderer>", StatusOK},
		{"missing", 404, "<h1>Not Found</h1>", StatusNotFound},
		{"inert", 404, "<h1>Not Yet Enabled</h1><p>" + inertMarker + " Its source is not readable.</p>", StatusInert},
		{"upstream broken", 502, "bad gateway", StatusError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			p, err := DoProbe(context.Background(), srv.Client(), srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if p.State != tt.want {
				t.Fatalf("state = %q, want %q", p.State, tt.want)
			}
			if len(p.Freshness) != 64 {
				t.Fatalf("freshness = %q, want a sha256 hex digest", p.Freshness)
			}
		})
	}
}

// The freshness hash is the cache key, so it has to move when the body moves
// and stay put when it does not. A realm's render is a function of chain state
// and changes with no URL change, which is why the URL is not the key.
func TestFreshnessTracksTheBodyAndNotTheURL(t *testing.T) {
	body := "one"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()
	ctx := context.Background()
	a, err := DoProbe(ctx, srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DoProbe(ctx, srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if a.Freshness != b.Freshness {
		t.Fatal("same body produced two freshness hashes")
	}
	body = "two"
	c, err := DoProbe(ctx, srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if c.Freshness == a.Freshness {
		t.Fatal("changed body produced the same freshness hash")
	}
}

func TestNegativeTTLIsShortestForTheStateThatFlipsOnItsOwn(t *testing.T) {
	// A package waiting to be enabled becomes a real render with no request
	// from us, so it must be re-probed far sooner than one that does not exist.
	if StatusInert.NegativeTTL() >= StatusNotFound.NegativeTTL() {
		t.Fatalf("inert TTL %v must be shorter than not-found %v",
			StatusInert.NegativeTTL(), StatusNotFound.NegativeTTL())
	}
	if StatusOK.NegativeTTL() != 0 {
		t.Fatal("a healthy page has no negative TTL")
	}
}

func TestRungByName(t *testing.T) {
	tests := []struct {
		size string
		dpr  int
		want string
		ok   bool
	}{
		{"", 0, "og", true},
		{"og", 2, "og", true},
		{"hero", 1, "hero@1x", true},
		{"hero", 2, "hero@2x", true},
		{"thumb", 0, "thumb@2x", true}, // an unstated dpr is the retina one
		{"thumb@1x", 2, "thumb@1x", true},
		{"avatar", 1, "", false}, // cut on purpose, and must not silently resolve
		{"enormous", 1, "", false},
	}
	for _, tt := range tests {
		got, ok := RungByName(tt.size, tt.dpr)
		if ok != tt.ok {
			t.Fatalf("RungByName(%q,%d) ok = %v, want %v", tt.size, tt.dpr, ok, tt.ok)
		}
		if ok && got.Name != tt.want {
			t.Fatalf("RungByName(%q,%d) = %q, want %q", tt.size, tt.dpr, got.Name, tt.want)
		}
	}
}

// Every rung crops from the top, because a realm render's first screenful is
// what identifies it. Centring a 3,000-pixel render lands the thumbnail
// somewhere in the middle of a table.
func TestCoverTopKeepsTheTopLeftAndHitsTheExactSize(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 800, 4000))
	for y := 0; y < 4000; y++ {
		c := color.RGBA{0, 0, 255, 255} // bottom: blue
		if y < 200 {
			c = color.RGBA{255, 0, 0, 255} // top: red
		}
		for x := 0; x < 800; x++ {
			src.SetRGBA(x, y, c)
		}
	}
	for _, r := range Rungs {
		dst := CoverTop(src, r.W, r.H)
		if got := dst.Bounds().Dx(); got != r.W {
			t.Fatalf("%s width = %d, want %d", r.Name, got, r.W)
		}
		if got := dst.Bounds().Dy(); got != r.H {
			t.Fatalf("%s height = %d, want %d", r.Name, got, r.H)
		}
		cr, _, cb, _ := dst.At(2, 2).RGBA()
		if cr <= cb {
			t.Fatalf("%s: top-left pixel is not from the top of the source", r.Name)
		}
	}
}

func TestCoverTopSurvivesADegenerateSource(t *testing.T) {
	// A zero-area element is the case that wedged a worker for a full deadline
	// before the area guard existed; the ladder must not panic on one either.
	empty := image.NewRGBA(image.Rect(0, 0, 0, 0))
	if got := CoverTop(empty, 160, 90).Bounds(); got.Dx() != 160 || got.Dy() != 90 {
		t.Fatalf("bounds = %v, want 160x90", got)
	}
}

func TestSplitPathDropsTheVersionSegment(t *testing.T) {
	tests := []struct {
		in, leaf, prefix string
	}{
		{"gno.land/r/moul/config/v0", "config", "r/moul/"},
		{"gno.land/p/nt/avl/v0", "avl", "p/nt/"},
		{"gno.land/r/gov/dao", "dao", "r/gov/"},
		{"gno.land/r/moul/v1beta", "v1beta", "r/moul/"}, // not a version, not dropped
		{"gno.land", "gno.land", ""},
	}
	for _, tt := range tests {
		leaf, prefix := splitPath(tt.in)
		if leaf != tt.leaf || prefix != tt.prefix {
			t.Fatalf("splitPath(%q) = (%q,%q), want (%q,%q)", tt.in, leaf, prefix, tt.leaf, tt.prefix)
		}
	}
}

// A tile is the answer to "there is no picture", so it has to be produced for
// every state and every rung without ever failing or coming back empty.
func TestTileRendersForEveryStateAndRung(t *testing.T) {
	for kind := range tileCopy {
		for _, r := range Rungs {
			b, err := EncodeTile("gno.land/r/moul/home", kind, r)
			if err != nil {
				t.Fatalf("%s at %s: %v", kind, r.Name, err)
			}
			if len(b) < 64 {
				t.Fatalf("%s at %s: %d bytes, that is not an image", kind, r.Name, len(b))
			}
		}
	}
}

func TestTileIsDeterministic(t *testing.T) {
	a, err := EncodeTile("gno.land/r/moul/home", TileParked, Rungs[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeTile("gno.land/r/moul/home", TileParked, Rungs[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("the same path and state produced two different tiles")
	}
	c, err := EncodeTile("gno.land/r/moul/other", TileParked, Rungs[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(c) {
		t.Fatal("two different paths produced the same tile")
	}
}

func TestTileCopyAlwaysHasAPlainLine(t *testing.T) {
	// The plain line is what a reader who does not know what a realm is gets,
	// and it is always in the alt text, so an empty one is a hole in the page
	// for exactly the reader this feature exists for.
	for kind := range tileCopy {
		short, plain := TileCopy(kind)
		if short == "" || plain == "" {
			t.Fatalf("%s: short=%q plain=%q", kind, short, plain)
		}
	}
	// An unknown kind must fall back rather than return two empty strings.
	short, plain := TileCopy(TileKind("invented"))
	if short == "" || plain == "" {
		t.Fatal("unknown tile kind returned no copy")
	}
}

func TestTileKindFor(t *testing.T) {
	tests := []struct {
		matched Matched
		state   Status
		want    TileKind
	}{
		{MatchedStatus, StatusInert, TileParked},
		{MatchedStatus, StatusNotFound, TileNone},
		{MatchedStatus, StatusError, TileError},
		{MatchedDirectory, StatusOK, TileDirectory},
		{MatchedStatus, StatusOK, TileNone},
	}
	for _, tt := range tests {
		if got := TileKindFor(tt.matched, tt.state); got != tt.want {
			t.Fatalf("TileKindFor(%q,%q) = %q, want %q", tt.matched, tt.state, got, tt.want)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got, err := st.Get("https://gno.land/r/gov/dao", ThemeLight); err != nil || got != nil {
		t.Fatalf("Get on an empty store = (%v,%v), want (nil,nil)", got, err)
	}

	e := &Entry{
		URL: "https://gno.land/r/gov/dao", Theme: ThemeLight,
		Matched: MatchedRealm, Selector: "md-renderer.c-realm-view",
		UpstreamStatus: 200, State: StatusOK, Freshness: "abc",
		CapturedAt: time.Now().UTC().Truncate(time.Second),
		ProbedAt:   time.Now().UTC().Truncate(time.Second),
		Object:     "0011223344556677",
		Rungs:      map[string]int{"og.webp": 16570},
		Modes:      map[string]string{"render": "master-render.webp"},
	}
	e.Box.W, e.Box.H = 816, 1358
	if err := st.Put(e); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(e.URL, ThemeLight)
	if err != nil {
		t.Fatal(err)
	}
	if got.Matched != MatchedRealm || got.Box.W != 816 || got.Rungs["og.webp"] != 16570 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if !got.HasImage() {
		t.Fatal("HasImage() is false for a captured realm")
	}

	// The same URL in the other theme is a different picture and must not
	// collide with this one.
	if other, err := st.Get(e.URL, ThemeDark); err != nil || other != nil {
		t.Fatalf("dark theme read the light entry: %v %v", other, err)
	}

	if err := st.PutFile(e.Object, "og.webp", []byte("bytes")); err != nil {
		t.Fatal(err)
	}
	b, err := st.ReadFile(e.Object, "og.webp")
	if err != nil || string(b) != "bytes" {
		t.Fatalf("ReadFile = (%q,%v)", b, err)
	}

	stats, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Entries != 1 || stats.Captured != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestStoreStatsOnAnEmptyStore(t *testing.T) {
	// SUM() over no rows is NULL, which is the kind of thing that only fails on
	// a freshly deployed box.
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Stats(); err != nil {
		t.Fatal(err)
	}
}

func TestObjectKeyChangesWithFreshnessAndTheme(t *testing.T) {
	a := ObjectKey("https://gno.land/r/gov/dao", ThemeLight, "f1")
	if b := ObjectKey("https://gno.land/r/gov/dao", ThemeLight, "f1"); a != b {
		t.Fatal("object key is not deterministic")
	}
	if b := ObjectKey("https://gno.land/r/gov/dao", ThemeLight, "f2"); a == b {
		t.Fatal("a changed page reused its object directory")
	}
	if b := ObjectKey("https://gno.land/r/gov/dao", ThemeDark, "f1"); a == b {
		t.Fatal("the dark capture would overwrite the light one")
	}
}

func TestPathOfStripsGnowebSuffixes(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://gno.land/r/gov/dao", "gno.land/r/gov/dao"},
		{"https://gno.land/r/gov/dao$source", "gno.land/r/gov/dao"},
		{"https://gno.land/r/gnoland/blog:p/hello", "gno.land/r/gnoland/blog"},
		{"https://gno.land/", "gno.land"},
	}
	for _, tt := range tests {
		if got := pathOf(tt.in); got != tt.want {
			t.Fatalf("pathOf(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPageURL(t *testing.T) {
	tests := []struct{ base, path, want string }{
		{"https://gno.land", "gno.land/r/gov/dao", "https://gno.land/r/gov/dao"},
		{"https://gno.land/", "r/gov/dao", "https://gno.land/r/gov/dao"},
		{"https://test6.testnets.gno.land", "gno.land/p/moul/txlink", "https://test6.testnets.gno.land/p/moul/txlink"},
	}
	for _, tt := range tests {
		if got := PageURL(tt.base, tt.path); got != tt.want {
			t.Fatalf("PageURL(%q,%q) = %q, want %q", tt.base, tt.path, got, tt.want)
		}
	}
}

// A queued capture must be scheduled once however many readers ask for it:
// fifty simultaneous requests for a cold realm are one capture, not fifty.
func TestEnqueueDeduplicates(t *testing.T) {
	svc, err := New(Config{Root: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	for i := 0; i < 50; i++ {
		if !svc.Enqueue("https://gno.land/r/gov/dao", ThemeLight, true) {
			t.Fatal("Enqueue refused")
		}
	}
	if n := len(svc.interactive); n != 1 {
		t.Fatalf("queued %d jobs, want 1", n)
	}
}

func TestServeOnAColdStoreQueuesAndAnswersWithATile(t *testing.T) {
	svc, err := New(Config{Root: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	rung, _ := RungByName("thumb", 2)
	body, entry, status := svc.Serve("https://gno.land/r/gov/dao", ThemeLight, ModeRender, rung)
	if status != 202 {
		t.Fatalf("status = %d, want 202", status)
	}
	if entry != nil {
		t.Fatal("a cold store returned an entry")
	}
	// Never a broken image and never a blank one, even on the very first
	// request for a path nothing has ever captured.
	if len(body) < 64 {
		t.Fatalf("body = %d bytes, want a tile", len(body))
	}
	if n := len(svc.interactive); n != 1 {
		t.Fatalf("queued %d, want 1", n)
	}
}

func TestServeAnInertEntryReturnsTheParkedTileNotAnError(t *testing.T) {
	svc, err := New(Config{Root: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	e := &Entry{
		URL: "https://gno.land/r/x/parked", Theme: ThemeLight,
		Matched: MatchedStatus, State: StatusInert, UpstreamStatus: 404,
		Freshness: "f", ProbedAt: time.Now().UTC(),
	}
	if err := svc.Store().Put(e); err != nil {
		t.Fatal(err)
	}
	rung, _ := RungByName("og", 1)
	body, got, status := svc.Serve(e.URL, ThemeLight, ModeRender, rung)
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if got == nil || got.State != StatusInert {
		t.Fatalf("entry = %+v", got)
	}
	if len(body) < 64 {
		t.Fatal("no tile body")
	}
}

func TestServerRefusesAHostOffTheAllowlist(t *testing.T) {
	svc, err := New(Config{Root: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	mux := http.NewServeMux()
	NewServer(svc, "").Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// An unbounded ?url= is an open proxy and a way to spend somebody else's
	// CPU on headless Chrome.
	resp, err := srv.Client().Get(srv.URL + "/shot?url=https://evil.example/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServerInvalidateIsClosedWithoutAToken(t *testing.T) {
	svc, err := New(Config{Root: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	mux := http.NewServeMux()
	NewServer(svc, "").Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL+"/invalidate", "application/json",
		stringsReader(`{"url":"https://gno.land/r/gov/dao"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when no admin token is configured", resp.StatusCode)
	}
}

func TestServerTileServesEveryKind(t *testing.T) {
	svc, err := New(Config{Root: t.TempDir(), Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	mux := http.NewServeMux()
	NewServer(svc, "").Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for kind := range tileCopy {
		resp, err := srv.Client().Get(srv.URL + "/tile?path=gno.land/r/moul/home&size=thumb&dpr=1&kind=" + string(kind))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", kind, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "image/webp" {
			t.Fatalf("%s: content-type = %q", kind, ct)
		}
	}
}

func stringsReader(s string) io.Reader { return strings.NewReader(s) }
