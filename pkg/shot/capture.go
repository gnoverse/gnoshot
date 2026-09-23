package shot

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const (
	// ViewportW and ViewportH are the CSS-pixel viewport every capture is taken
	// at. Wide enough that gnoweb lays out at its desktop breakpoint, which is
	// the layout a shared card should show.
	ViewportW = 1280
	ViewportH = 860

	// MasterDSF is the device scale factor of the master capture. Not 3: at
	// DSF 3 lossless WebP's advantage over AVIF collapses, the capture costs
	// 5.8 s against 4.5 s, and no rung below needs more than 1280 physical
	// pixels of width.
	MasterDSF = 2

	// MaxCSSHeight caps the master. The worst case measured on live gnoweb is
	// /r/gov/dao$source at 10,296 CSS px and 3.15 MB of PNG, and nothing in the
	// product ever shows a page that tall. Over the cap the top is captured and
	// the entry is flagged truncated; the notice is a consumer's to draw and is
	// deliberately not burned into the pixels, because the same master feeds
	// og:image where a watermark would be noise.
	MaxCSSHeight = 4000

	// CaptureDeadline is the per-URL worker deadline.
	CaptureDeadline = 30 * time.Second
)

// Master is one captured image, before the ladder.
type Master struct {
	Mode      Mode
	Img       image.Image
	Box       Box
	Truncated bool
}

// Result is everything one visit to a page produced.
type Result struct {
	Matched  Matched
	Selector string
	Box      Box
	Masters  map[Mode]*Master
}

// Browser owns one long-lived headless Chrome and one reused tab.
//
// Reusing the tab is not a micro-optimisation. A fresh chromedp.NewContext per
// URL measured 68.8 s and 66.4 s for two pages that take 5.9 s and 3.9 s in a
// reused tab. The root cause was never isolated; it is recorded because the fix
// is one line and the symptom looks like a network stall.
type Browser struct {
	mu        sync.Mutex
	allocCtx  context.Context
	allocStop context.CancelFunc
	tabCtx    context.Context
	tabStop   context.CancelFunc
	execPath  string
	captures  int
	// Recycle after this many captures. Chrome leaks over long runs and
	// ~1.3 GB per worker leaves no room to find that out the hard way.
	recycleEvery int
}

// ChromePath finds a usable Chrome, preferring an explicit override.
func ChromePath() string {
	if p := os.Getenv("GNOSHOT_CHROME"); p != "" {
		return p
	}
	for _, c := range []string{
		"/usr/bin/chromium", "/usr/bin/chromium-browser", "/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable", "/snap/bin/chromium",
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	// Playwright's download, which is what a developer machine usually has.
	if home, err := os.UserHomeDir(); err == nil {
		matches, _ := filepath.Glob(home + "/.cache/ms-playwright/chromium-*/chrome-linux64/chrome")
		if len(matches) > 0 {
			return matches[len(matches)-1]
		}
		matches, _ = filepath.Glob(home + "/.cache/ms-playwright/chromium-*/chrome-linux/chrome")
		if len(matches) > 0 {
			return matches[len(matches)-1]
		}
	}
	return ""
}

// NewBrowser launches Chrome. The caller owns Close.
func NewBrowser(parent context.Context, recycleEvery int) (*Browser, error) {
	b := &Browser{execPath: ChromePath(), recycleEvery: recycleEvery}
	if b.recycleEvery <= 0 {
		b.recycleEvery = 200
	}
	if err := b.start(parent); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Browser) start(parent context.Context) error {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.DisableGPU,
		chromedp.NoSandbox,
		chromedp.Flag("headless", "new"),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WindowSize(ViewportW, ViewportH),
	)
	if b.execPath != "" {
		opts = append(opts, chromedp.ExecPath(b.execPath))
	}
	b.allocCtx, b.allocStop = chromedp.NewExecAllocator(parent, opts...)
	b.tabCtx, b.tabStop = chromedp.NewContext(b.allocCtx)
	// Force the tab to actually exist now, so a launch failure surfaces here
	// rather than inside the first capture.
	if err := chromedp.Run(b.tabCtx); err != nil {
		b.Close()
		return fmt.Errorf("launch chrome (%s): %w", b.execPath, err)
	}
	b.captures = 0
	return nil
}

// Close shuts the browser down. An orphaned headless Chrome holding over a
// gigabyte survives its parent and is easy to miss in ps output.
func (b *Browser) Close() {
	if b.tabStop != nil {
		b.tabStop()
	}
	if b.allocStop != nil {
		b.allocStop()
	}
}

// Capture visits one URL and produces the masters for the requested modes.
//
// The probe has already decided whether the page is worth visiting; this is
// only ever called for a page that answered 200.
// settleJS resolves once the DOM has stopped changing, or at the cap.
//
// fonts.ready plus two frames is enough for gnoweb, which server-renders and
// then hydrates. It is not enough for a page that paints nothing until its
// JavaScript has fetched and rendered, and those are pages this service is now
// also asked to photograph. Measured 2026-09-23 in page mode, all three
// answering 200 with real content in a browser: gnoswap.io came out a
// 228-byte blank, gnolove.world and kourt.xyz came out as their own loading
// spinners. A confident picture of a spinner is worse than no picture, because
// nothing downstream can tell the two apart.
//
// A MutationObserver quiesce rather than a fixed sleep or network idle:
//
//   - A fixed sleep taxes every gnoweb capture for the sake of the handful that
//     need it, and the sweep runs over every realm on the chain. This returns
//     in one quiet window on a page that was already done.
//   - Network idle never arrives on a page holding a websocket or a poll open,
//     and several of these do.
//
// Quiet means the whole document, subtree and attributes included, because a
// spinner that swaps one class is a page still deciding what it is. The cap
// bounds the worst case, a page that never stops animating: it fires and the
// capture proceeds, which is exactly what happened before this existed.
const (
	settleQuiet = 400  // ms without a mutation that counts as done
	settleCap   = 2500 // ms after which we photograph whatever is there
)

var settleJS = fmt.Sprintf(`new Promise(resolve => {
  let timer = null;
  const done = () => { obs.disconnect(); clearTimeout(timer); clearTimeout(cap); resolve(true); };
  const cap = setTimeout(done, %d);
  const bump = () => { clearTimeout(timer); timer = setTimeout(done, %d); };
  const obs = new MutationObserver(bump);
  obs.observe(document.documentElement, {childList: true, subtree: true, attributes: true, characterData: true});
  bump();
})`, settleCap, settleQuiet)

// Capture photographs one page.
//
// site says the URL is on a host that does not serve gnoweb, so the selector
// chain is skipped and the whole document is the picture. See docHeightJS.
func (b *Browser) Capture(ctx context.Context, pageURL string, theme Theme, modes []Mode, site bool) (*Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.captures >= b.recycleEvery {
		parent := b.allocCtx
		_ = parent
		b.Close()
		if err := b.start(context.Background()); err != nil {
			return nil, fmt.Errorf("recycle: %w", err)
		}
	}
	b.captures++

	ctx, cancel := context.WithTimeout(ctx, CaptureDeadline)
	defer cancel()
	tab, tabCancel := context.WithCancel(b.tabCtx)
	defer tabCancel()
	// Propagate the deadline into the tab context without replacing it: the tab
	// has to outlive this call, the deadline must not.
	go func() {
		select {
		case <-ctx.Done():
			tabCancel()
		case <-tab.Done():
		}
	}()

	var res struct {
		Matched  Matched `json:"matched"`
		Selector string  `json:"selector"`
		X        float64 `json:"x"`
		Y        float64 `json:"y"`
		W        float64 `json:"w"`
		H        float64 `json:"h"`
		Doc      float64 `json:"doc"`
	}
	awaitPromise := func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	}
	probeScript := resolveJS
	if site {
		probeScript = docHeightJS
	}
	tasks := chromedp.Tasks{
		// Device metrics stay at scale 1 and the master's 2x comes from the
		// screenshot clip. Setting both is how you silently get a 4x image.
		emulation.SetDeviceMetricsOverride(ViewportW, ViewportH, 1, false),
		chromedp.Navigate(pageURL),
		// The theme is set on the DOM, not through the cookie. Network.setCookie
		// produced a byte-identical light capture in testing, and the cookie
		// route over HTTP is additionally poisoned by the CDN in front of
		// gnoweb, which caches the HTML and does not vary on it.
		chromedp.Evaluate(fmt.Sprintf(
			`document.documentElement.setAttribute('data-theme', %q)`, string(theme)), nil),
		// gnoweb loads every controller as an ES module and a frame grabbed
		// before they settle is unstyled text, which looks almost right and is
		// therefore worse than looking broken.
		chromedp.Evaluate(
			`new Promise(r => document.fonts.ready.then(() => requestAnimationFrame(() => requestAnimationFrame(() => r(true)))))`,
			nil, awaitPromise),
		// And then wait for the DOM to stop moving, which fonts.ready does not
		// imply on a page that renders itself in JavaScript.
		chromedp.Evaluate(settleJS, nil, awaitPromise),
		chromedp.Evaluate(probeScript, &res),
	}
	if err := chromedp.Run(tab, tasks); err != nil {
		return nil, fmt.Errorf("load %s: %w", pageURL, err)
	}

	out := &Result{
		Matched:  res.Matched,
		Selector: res.Selector,
		Box:      Box{X: res.X, Y: res.Y, W: res.W, H: res.H},
		Masters:  map[Mode]*Master{},
	}

	for _, m := range modes {
		var clip Box
		switch m {
		case ModeRender:
			// Nothing to crop to on either: an error page has no render, and a
			// site host never ran the chain that would have found one. Both
			// are answered by the page master, which the ladder then derives
			// every rung from.
			if out.Matched == MatchedStatus || out.Matched == MatchedSite {
				continue
			}
			clip = out.Box
		case ModePage:
			clip = Box{X: 0, Y: 0, W: ViewportW, H: math.Max(res.Doc, ViewportH)}
		}
		truncated := false
		if clip.H > MaxCSSHeight {
			clip.H = MaxCSSHeight
			truncated = true
		}
		buf, err := b.shoot(tab, clip)
		if err != nil {
			return nil, fmt.Errorf("capture %s: %w", m, err)
		}
		img, err := png.Decode(bytes.NewReader(buf))
		if err != nil {
			return nil, fmt.Errorf("decode %s master: %w", m, err)
		}
		out.Masters[m] = &Master{Mode: m, Img: img, Box: clip, Truncated: truncated}
	}
	return out, nil
}

// shoot takes one clipped screenshot in CSS pixels at the master DSF.
func (b *Browser) shoot(ctx context.Context, clip Box) ([]byte, error) {
	var buf []byte
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		buf, err = page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatPng).
			WithCaptureBeyondViewport(true).
			WithClip(&page.Viewport{
				X: clip.X, Y: clip.Y, Width: clip.W, Height: clip.H, Scale: MasterDSF,
			}).Do(ctx)
		return err
	}))
	return buf, err
}
