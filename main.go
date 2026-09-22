// Command gnoshot captures pictures of gno.land pages.
//
// The whole gnoweb page, and the crop that actually matters: the realm's own
// rendered output. Run it as a service, or take one capture from the CLI; both
// go through the same code path, so a picture that is wrong in production can
// be reproduced with one command and no service running.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image/png"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gnoverse/gnoshot/pkg/shot"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("gnoshot: ")
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: gnoshot <command> [flags]

  serve      run the capture service
  capture    capture one URL and write the ladder to a directory
  resolve    print which selector matches a URL, and its box, without capturing
  ladder     derive the ladder from an existing master image
  sweep      enqueue every known path against a running service
  tile       render one placeholder tile

Run "gnoshot <command> -h" for the flags of one command.
`)
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("no command")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return cmdServe(rest)
	case "capture":
		return cmdCapture(rest)
	case "resolve":
		return cmdResolve(rest)
	case "ladder":
		return cmdLadder(rest)
	case "sweep":
		return cmdSweep(rest)
	case "tile":
		return cmdTile(rest)
	case "-h", "--help", "help":
		usage()
		return nil
	}
	usage()
	return fmt.Errorf("unknown command %q", cmd)
}

// defaultRoot follows the XDG layout rather than inventing a dotdir.
func defaultRoot() string {
	if v := os.Getenv("GNOSHOT_ROOT"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "gnoshot")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./gnoshot-data"
	}
	return filepath.Join(home, ".cache", "gnoshot")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", env("GNOSHOT_ADDR", ":8890"), "listen address")
	root := fs.String("root", defaultRoot(), "storage root")
	workers := fs.Int("workers", 2, "capture workers; each holds a browser at roughly 1.3 GB")
	allow := fs.String("allow", shot.DefaultAllowHosts, "comma-separated host allowlist")
	base := fs.String("gnoweb", env("GNOSHOT_GNOWEB", "https://gno.land"), "gnoweb base URL for the sweep")
	source := fs.String("source", env("GNOSHOT_SOURCE", ""), "mygnoscan base URL to enumerate paths from; empty disables the corpus sweep")
	network := fs.String("network", env("GNOSHOT_NETWORK", "mainnet"), "network id to enumerate")
	sweep := fs.Duration("sweep", 6*time.Hour, "sweep interval; 0 disables")
	refresh := fs.Duration("refresh", 15*time.Minute, "how stale a probe may get before a read re-probes")
	token := fs.String("admin-token", os.Getenv("GNOSHOT_ADMIN_TOKEN"), "bearer token for /invalidate; empty refuses it")
	fs.Parse(args)

	svc, err := shot.New(shot.Config{
		Root: *root, Workers: *workers, AllowHosts: *allow,
		RefreshAfter: *refresh, SweepEvery: *sweep, SweepSource: *source,
		SweepNetwork: *network, GnowebBase: *base,
	})
	if err != nil {
		return err
	}
	defer svc.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.Run(ctx)

	mux := http.NewServeMux()
	shot.NewServer(svc, *token).Routes(mux)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sd)
	}()
	log.Printf("listening on %s, root %s, chrome %s", *addr, *root, shot.ChromePath())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func cmdCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	out := fs.String("out", ".", "output directory")
	theme := fs.String("theme", "light", "light|dark")
	modes := fs.String("mode", "both", "page|render|both")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: gnoshot capture [-out DIR] [-theme light|dark] [-mode page|render|both] <url>")
	}
	pageURL := fs.Arg(0)
	th, err := shot.ParseTheme(*theme)
	if err != nil {
		return err
	}
	var want []shot.Mode
	switch *modes {
	case "both":
		want = []shot.Mode{shot.ModeRender, shot.ModePage}
	default:
		m, err := shot.ParseMode(*modes)
		if err != nil {
			return err
		}
		want = []shot.Mode{m}
	}

	ctx := context.Background()
	p, err := shot.DoProbe(ctx, shot.DefaultProbeClient(), pageURL)
	if err != nil {
		return err
	}
	fmt.Printf("probe   status=%d state=%s freshness=%s\n", p.Status, p.State, p.Freshness[:16])
	if p.State != shot.StatusOK {
		kind := shot.TileKindFor(shot.MatchedStatus, p.State)
		short, plain := shot.TileCopy(kind)
		fmt.Printf("tile    %s: %s (%s)\n", kind, short, plain)
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return err
		}
		for _, r := range shot.Rungs {
			b, err := shot.EncodeTile(pathOfURL(pageURL), kind, r)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(*out, r.Name+".webp"), b, 0o644); err != nil {
				return err
			}
			fmt.Printf("  %-14s %7d bytes\n", r.Name+".webp", len(b))
		}
		return nil
	}

	br, err := shot.NewBrowser(ctx, 200)
	if err != nil {
		return err
	}
	defer br.Close()
	t0 := time.Now()
	res, err := br.Capture(ctx, pageURL, th, want)
	if err != nil {
		return err
	}
	fmt.Printf("capture %v matched=%s selector=%s box=%.0fx%.0f\n",
		time.Since(t0).Round(time.Millisecond), res.Matched, res.Selector, res.Box.W, res.Box.H)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	for mode, m := range res.Masters {
		b, err := shot.EncodeMaster(m.Img)
		if err != nil {
			return err
		}
		name := "master-" + string(mode) + ".webp"
		if err := os.WriteFile(filepath.Join(*out, name), b, 0o644); err != nil {
			return err
		}
		fmt.Printf("  %-18s %7d bytes  %v truncated=%v\n", name, len(b), m.Img.Bounds().Size(), m.Truncated)
	}
	src := res.Masters[shot.ModeRender]
	if src == nil {
		src = res.Masters[shot.ModePage]
	}
	if src == nil {
		return nil
	}
	l, err := shot.Ladder(src.Img)
	if err != nil {
		return err
	}
	for _, e := range l {
		if err := os.WriteFile(filepath.Join(*out, e.Name), e.Bytes, 0o644); err != nil {
			return err
		}
		fmt.Printf("  %-18s %7d bytes\n", e.Name, len(e.Bytes))
	}
	return nil
}

func cmdResolve(args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ExitOnError)
	theme := fs.String("theme", "light", "light|dark")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: gnoshot resolve <url> [url...]")
	}
	th, err := shot.ParseTheme(*theme)
	if err != nil {
		return err
	}
	ctx := context.Background()
	br, err := shot.NewBrowser(ctx, 200)
	if err != nil {
		return err
	}
	defer br.Close()
	client := shot.DefaultProbeClient()
	fmt.Printf("%-4s %-9s %-26s %-11s %s\n", "HTTP", "STATE", "SELECTOR", "BOX", "URL")
	for _, u := range fs.Args() {
		p, err := shot.DoProbe(ctx, client, u)
		if err != nil {
			fmt.Printf("%-4s %-9s %-26s %-11s %s\n", "err", "-", "-", "-", u)
			continue
		}
		if p.State != shot.StatusOK {
			fmt.Printf("%-4d %-9s %-26s %-11s %s\n", p.Status, p.State, "-", "-", u)
			continue
		}
		// An empty mode list resolves the chain and captures nothing.
		res, err := br.Capture(ctx, u, th, nil)
		if err != nil {
			fmt.Printf("%-4d %-9s %-26s %-11s %s\n", p.Status, "err", "-", "-", u)
			continue
		}
		sel := res.Selector
		if sel == "" {
			sel = "(none)"
		}
		fmt.Printf("%-4d %-9s %-26s %4.0fx%-6.0f %s\n", p.Status, res.Matched, sel, res.Box.W, res.Box.H, u)
	}
	return nil
}

func cmdLadder(args []string) error {
	fs := flag.NewFlagSet("ladder", flag.ExitOnError)
	out := fs.String("out", ".", "output directory")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: gnoshot ladder [-out DIR] <master.png>")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return fmt.Errorf("decode (ladder reads PNG masters): %w", err)
	}
	l, err := shot.Ladder(img)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	for _, e := range l {
		if err := os.WriteFile(filepath.Join(*out, e.Name), e.Bytes, 0o644); err != nil {
			return err
		}
		fmt.Printf("%-18s %7d bytes\n", e.Name, len(e.Bytes))
	}
	return nil
}

func cmdSweep(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ExitOnError)
	service := fs.String("service", env("GNOSHOT_ADDR_URL", "http://127.0.0.1:8890"), "running gnoshot service")
	token := fs.String("admin-token", os.Getenv("GNOSHOT_ADMIN_TOKEN"), "bearer token")
	source := fs.String("source", env("GNOSHOT_SOURCE", "https://mygnoscan.moul.p2p.team"), "mygnoscan base URL")
	network := fs.String("network", env("GNOSHOT_NETWORK", "mainnet"), "network id")
	base := fs.String("gnoweb", env("GNOSHOT_GNOWEB", "https://gno.land"), "gnoweb base URL")
	fs.Parse(args)

	ctx := context.Background()
	client := shot.DefaultProbeClient()
	paths, err := shot.FetchPaths(ctx, client, *source, *network)
	if err != nil {
		return err
	}
	fmt.Printf("%d paths from %s\n", len(paths), *source)
	queued := 0
	for _, p := range paths {
		body, _ := json.Marshal(map[string]string{"url": shot.PageURL(*base, p)})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(*service, "/")+"/invalidate", strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+*token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			queued++
		}
	}
	fmt.Printf("queued %d\n", queued)
	return nil
}

func cmdTile(args []string) error {
	fs := flag.NewFlagSet("tile", flag.ExitOnError)
	out := fs.String("out", ".", "output directory")
	kind := fs.String("kind", "none", "parked|pure|no-render|directory|none|error")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: gnoshot tile [-kind K] [-out DIR] <package-path>")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	short, plain := shot.TileCopy(shot.TileKind(*kind))
	fmt.Printf("%s: %s (%s)\n", *kind, short, plain)
	for _, r := range shot.Rungs {
		b, err := shot.EncodeTile(fs.Arg(0), shot.TileKind(*kind), r)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*out, r.Name+".webp"), b, 0o644); err != nil {
			return err
		}
		fmt.Printf("  %-14s %7d bytes\n", r.Name+".webp", len(b))
	}
	return nil
}

func pathOfURL(u string) string {
	i := strings.Index(u, "://")
	if i < 0 {
		return u
	}
	return strings.SplitN(u[i+3:], "?", 2)[0]
}
