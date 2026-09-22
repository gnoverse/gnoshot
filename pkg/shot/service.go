package shot

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Config is everything the service needs to run.
type Config struct {
	Root         string        // storage root
	Workers      int           // browsers, each ~1.3 GB of RSS
	AllowHosts   string        // comma-separated allowlist
	RefreshAfter time.Duration // how old a probe may be before a read re-probes
	SweepEvery   time.Duration // background sweep interval, 0 to disable
	SweepSource  string        // mygnoscan-compatible base URL to enumerate paths from
	SweepNetwork string
	GnowebBase   string // e.g. https://gno.land
	QueueDepth   int
	RecycleEvery int
}

// Defaults fills the zero values with the sizes measured in the design.
func (c *Config) Defaults() {
	if c.Workers <= 0 {
		// RAM, not CPU, is the constraint: ~1.3 GB per worker.
		c.Workers = 2
	}
	if c.AllowHosts == "" {
		c.AllowHosts = DefaultAllowHosts
	}
	if c.RefreshAfter <= 0 {
		// A realm whose render reads the clock would otherwise re-capture on
		// every view. Fifteen minutes is the floor a chain-state cache key
		// still needs, because the probe is what detects the change and the
		// probe is not free either.
		c.RefreshAfter = 15 * time.Minute
	}
	if c.QueueDepth <= 0 {
		c.QueueDepth = 512
	}
	if c.GnowebBase == "" {
		c.GnowebBase = "https://gno.land"
	}
	if c.SweepNetwork == "" {
		c.SweepNetwork = "mainnet"
	}
	if c.RecycleEvery <= 0 {
		c.RecycleEvery = 200
	}
}

type job struct {
	url   string
	theme Theme
	// modes is what this job should capture. The sweep asks for the render
	// only: it is the one every rung is derived from, and it is 2.4x fewer
	// pixels than the page, which is the whole cost of a capture. The page
	// master is captured when somebody actually asks for one.
	modes []Mode
}

// Service owns the store, the queue and the browsers.
type Service struct {
	cfg   Config
	store *Store
	allow *Allowlist
	probe *http.Client

	interactive chan job
	background  chan job

	mu       sync.Mutex
	inFlight map[string]bool

	started time.Time
	wg      sync.WaitGroup
}

// New builds a service. Call Run to start the workers.
func New(cfg Config) (*Service, error) {
	cfg.Defaults()
	st, err := OpenStore(cfg.Root)
	if err != nil {
		return nil, err
	}
	return &Service{
		cfg:         cfg,
		store:       st,
		allow:       NewAllowlist(cfg.AllowHosts),
		probe:       DefaultProbeClient(),
		interactive: make(chan job, cfg.QueueDepth),
		background:  make(chan job, cfg.QueueDepth),
		inFlight:    map[string]bool{},
		started:     time.Now(),
	}, nil
}

func (s *Service) Store() *Store     { return s.store }
func (s *Service) Allow() *Allowlist { return s.allow }
func (s *Service) Config() Config    { return s.cfg }
func (s *Service) Close() error      { return s.store.Close() }

// Run starts the workers and blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	for i := 0; i < s.cfg.Workers; i++ {
		s.wg.Add(1)
		go s.worker(ctx, i)
	}
	if s.cfg.SweepEvery > 0 {
		s.wg.Add(1)
		go s.sweeper(ctx)
	}
	<-ctx.Done()
	s.wg.Wait()
	return nil
}

// Enqueue schedules a capture, deduplicating by object key.
//
// Fifty simultaneous requests for a cold realm are one capture, not fifty: the
// dedupe is the difference between a link going viral and the box falling over.
func (s *Service) Enqueue(url string, theme Theme, interactive bool, modes ...Mode) bool {
	if len(modes) == 0 {
		modes = []Mode{ModeRender}
	}
	k := Key(url, theme)
	s.mu.Lock()
	if s.inFlight[k] {
		s.mu.Unlock()
		return true
	}
	s.inFlight[k] = true
	s.mu.Unlock()

	q := s.background
	if interactive {
		q = s.interactive
	}
	select {
	case q <- job{url: url, theme: theme, modes: modes}:
		return true
	default:
		s.mu.Lock()
		delete(s.inFlight, k)
		s.mu.Unlock()
		return false
	}
}

func (s *Service) worker(ctx context.Context, n int) {
	defer s.wg.Done()
	var br *Browser
	defer func() {
		if br != nil {
			br.Close()
		}
	}()
	for {
		var j job
		// Interactive first, unconditionally: a reader waiting on a page beats
		// a sweep refreshing something nobody has open.
		select {
		case <-ctx.Done():
			return
		case j = <-s.interactive:
		default:
			select {
			case <-ctx.Done():
				return
			case j = <-s.interactive:
			case j = <-s.background:
			}
		}
		if br == nil {
			var err error
			br, err = NewBrowser(ctx, s.cfg.RecycleEvery)
			if err != nil {
				log.Printf("worker %d: %v", n, err)
				s.done(j)
				time.Sleep(5 * time.Second)
				continue
			}
		}
		if err := s.Refresh(ctx, br, j.url, j.theme, j.modes...); err != nil {
			log.Printf("worker %d: %s: %v", n, j.url, err)
		}
		s.done(j)
	}
}

func (s *Service) done(j job) {
	s.mu.Lock()
	delete(s.inFlight, Key(j.url, j.theme))
	s.mu.Unlock()
}

// Refresh probes a URL and captures it if the probe says anything changed. It
// is the whole pipeline, and the CLI runs the identical code path so the
// service is not the only way to reproduce a capture.
func (s *Service) Refresh(ctx context.Context, br *Browser, pageURL string, theme Theme, modes ...Mode) error {
	if len(modes) == 0 {
		modes = []Mode{ModeRender}
	}
	p, err := DoProbe(ctx, s.probe, pageURL)
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	now := time.Now().UTC()
	prev, err := s.store.Get(pageURL, theme)
	if err != nil {
		return err
	}
	if prev != nil && prev.Freshness == p.Freshness && prev.Object != "" && hasModes(prev, modes) {
		// Unchanged, and already carrying everything this job asked for. This
		// is the common case and it costs one UPDATE.
		return s.store.Touch(pageURL, theme, now)
	}

	e := &Entry{
		URL: pageURL, Theme: theme,
		UpstreamStatus: p.Status, State: p.State,
		Freshness: p.Freshness, ProbedAt: now,
		Rungs: map[string]int{}, Modes: map[string]string{},
	}

	if p.State != StatusOK {
		// A transient upstream error must not overwrite a good picture: the
		// realm was fine a minute ago and will be again, and replacing its
		// thumbnail with an error tile is a worse answer than a slightly old
		// screenshot.
		if p.State == StatusError && prev != nil && prev.HasImage() {
			return s.store.Touch(pageURL, theme, now)
		}
		e.Matched = MatchedStatus
		return s.store.Put(e)
	}

	// Whatever was already captured for this generation is kept: a page master
	// taken earlier must not be thrown away by a render-only sweep.
	if prev != nil && prev.Freshness == p.Freshness {
		modes = union(modes, capturedModes(prev))
	}
	res, err := br.Capture(ctx, pageURL, theme, modes)
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	e.Matched = res.Matched
	e.Selector = res.Selector
	e.Box.W = int(res.Box.W)
	e.Box.H = int(res.Box.H)
	e.CapturedAt = now

	obj := ObjectKey(pageURL, theme, p.Freshness)
	for mode, m := range res.Masters {
		b, err := EncodeMaster(m.Img, mode)
		if err != nil {
			return err
		}
		name := "master-" + string(mode) + ".webp"
		if err := s.store.PutFile(obj, name, b); err != nil {
			return err
		}
		e.Modes[string(mode)] = name
		if m.Truncated {
			e.Truncated = true
		}
	}
	// The ladder is derived from the render when there is one, and from the
	// page when there is not: a directory listing has no realm output, but it
	// is still a picture of something real and beats a generated tile.
	src := res.Masters[ModeRender]
	if src == nil {
		src = res.Masters[ModePage]
	}
	if src != nil {
		l, err := Ladder(src.Img)
		if err != nil {
			return err
		}
		for _, enc := range l {
			if err := s.store.PutFile(obj, enc.Name, enc.Bytes); err != nil {
				return err
			}
			e.Rungs[enc.Name] = len(enc.Bytes)
		}
	}
	e.Object = obj
	return s.store.Put(e)
}

// Lookup returns what is known about a URL without scheduling anything.
func (s *Service) Lookup(pageURL string, theme Theme) (*Entry, error) {
	return s.store.Get(pageURL, theme)
}

// Serve resolves one image request.
//
// Stale-while-revalidate: a cached picture is served immediately and the
// re-probe happens behind it. A reader never waits on Chrome, which is the
// whole reason the queue exists.
func (s *Service) Serve(pageURL string, theme Theme, mode Mode, r Rung) (body []byte, e *Entry, status int) {
	e, err := s.store.Get(pageURL, theme)
	if err != nil {
		log.Printf("lookup %s: %v", pageURL, err)
	}
	if e == nil {
		s.Enqueue(pageURL, theme, true, mode)
		b, _ := EncodeTile(pathOf(pageURL), TileNone, r)
		return b, nil, http.StatusAccepted
	}
	if time.Since(e.ProbedAt) > s.refreshAfter(e) {
		s.Enqueue(pageURL, theme, false, mode)
	}
	if e.Object == "" {
		b, _ := EncodeTile(pathOf(pageURL), TileKindFor(e.Matched, e.State), r)
		return b, e, http.StatusOK
	}
	name := r.Name + ".webp"
	if mode == ModePage {
		name = "master-page.webp"
	}
	b, err := s.store.ReadFile(e.Object, name)
	if err != nil {
		// Either the manifest says there is an image and the disk disagrees, or
		// this is the first request for a page master the sweep never took.
		// Both are answered the same way: a tile now, a picture next time.
		s.Enqueue(pageURL, theme, true, mode)
		tb, _ := EncodeTile(pathOf(pageURL), TileNone, r)
		return tb, e, http.StatusAccepted
	}
	return b, e, http.StatusOK
}

// refreshAfter is the per-entry re-probe interval. A recorded negative state
// has its own, much shorter, TTL: a package waiting to be enabled flips to a
// real render with no request from us.
func (s *Service) refreshAfter(e *Entry) time.Duration {
	if ttl := e.State.NegativeTTL(); ttl > 0 {
		return ttl
	}
	return s.cfg.RefreshAfter
}

// hasModes reports whether an entry already carries a master for every mode.
func hasModes(e *Entry, modes []Mode) bool {
	for _, m := range modes {
		// The render is absent by design on a directory or status page, and
		// asking for it again would re-capture those on every read.
		if m == ModeRender && e.Matched != MatchedRealm && e.Matched != MatchedReadme {
			continue
		}
		if e.Modes[string(m)] == "" {
			return false
		}
	}
	return true
}

func capturedModes(e *Entry) []Mode {
	out := make([]Mode, 0, len(e.Modes))
	for m := range e.Modes {
		out = append(out, Mode(m))
	}
	return out
}

func union(a, b []Mode) []Mode {
	seen := map[Mode]bool{}
	out := make([]Mode, 0, len(a)+len(b))
	for _, m := range append(append([]Mode{}, a...), b...) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}
