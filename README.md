# gnoshot

Pictures of gno.land pages: the whole gnoweb page, and the crop that actually
matters, the realm's own rendered output.

Every gno.land link shared today previews as a bare text card, and every list of
realms in every explorer is a column of monospace paths. This turns both into
pictures.

```
gnoshot capture https://gno.land/r/gov/dao -out /tmp/dao
probe   status=200 state=ok freshness=7f0a6b4c2441348b
capture 1.511s matched=realm selector=md-renderer.c-realm-view box=816x1358
  master-render.webp    74944 bytes  (1632,2714) truncated=false
  master-page.webp     150064 bytes  (2560,3254) truncated=false
  og.webp               16570 bytes
  hero@1x.webp          10788 bytes
  hero@2x.webp          25644 bytes
  thumb@1x.webp          1230 bytes
  thumb@2x.webp          3640 bytes
```

## Install

```
go install github.com/gnoverse/gnoshot@latest
```

It needs a Chrome or Chromium on the box. It looks in the usual places and in
Playwright's download directory; `GNOSHOT_CHROME` overrides.

## Run it

```
gnoshot serve -root /var/lib/gnoshot -source https://mygnoscan.moul.p2p.team
```

| route | |
|---|---|
| `GET /shot?url=&mode=page\|render&size=og\|hero\|thumb&theme=light\|dark&dpr=1\|2&v=` | the image |
| `GET /meta?url=&theme=` | the manifest entry: which selector matched, the box, whether it was truncated |
| `GET /tile?path=&kind=&size=&dpr=` | a generated placeholder, no capture involved |
| `POST /invalidate` | `{"url": "..."}`, bearer token, forces a re-probe |
| `GET /healthz` | queue depth, workers, entries, oldest probe |

`/shot` never answers 404. A gno.land URL that does not resolve still gets an
image, because the consumer is an `<img>` and its only other option is the
broken-image glyph.

## What it captures

Two modes. `page` is the whole gnoweb page, chrome included. `render` is the
realm's own output, and it is the one worth showing.

The crop is a **chain, not a pinned selector**, resolved by first candidate with
a non-zero bounding box, and which one matched is part of the response:

| matched | selector | what it means |
|---|---|---|
| `realm` | `md-renderer.c-realm-view` | a realm render, the primary capture |
| `readme` | `md-renderer.c-readme-view` | a documented directory or pure package, still a good crop |
| `directory` | `article.b-directory` | an undocumented directory listing |
| `status` | *(nothing matched)* | an error, a 404, or a package not yet enabled |

Verified against live gnoweb on `gnoland-1`, 2026-09-22, and reproducible with
`make selectors`:

```
HTTP STATE     SELECTOR                   BOX         URL
200  realm     md-renderer.c-realm-view    816x1358   https://gno.land/r/gov/dao
200  readme    md-renderer.c-readme-view  1200x384    https://gno.land/r/moul/config/v0
200  directory article.b-directory        1200x185    https://gno.land/r/moul/config
200  directory article.b-directory        1200x185    https://gno.land/p/moul/txlink
200  realm     md-renderer.c-realm-view    816x1154   https://gno.land/u/moul
404  inert     -                          -           .../pixelgnomes
404  not_found -                          -           https://gno.land/r/does/not/exist12345
```

Three things that look like details and are not:

- **The class has to be pinned.** A bare `md-renderer` also matches `$source`
  and `$help`, neither of which is a realm render and both of which are
  thousands of pixels tall.
- **Non-zero area, not a null check.** `md-renderer.c-realm-view` has appeared
  on `/u/<user>` with width and height 0. Asking Chrome to photograph a
  zero-area node does not fail, it never returns, and one such URL in the queue
  holds a worker for the whole deadline.
- **404 is two different things.** A package that has been submitted and is not
  yet enabled answers 404 with the same full-screen box as one that does not
  exist. They are told apart by the body text, because the status code cannot,
  and the first gets a 5-minute negative TTL because it flips to a real render
  with no request from us.

## The ladder

Capture once at device scale factor 2, derive everything else. Re-rendering per
size spends a browser round trip to produce pixels a resize already has.

| rung | candidates | for |
|---|---|---|
| `og` | 1200x630 | `og:image` and `twitter:image`; crawlers fetch one fixed size |
| `hero` | 640x360, 1280x720 | a detail-page hero, `srcset` 1x/2x |
| `thumb` | 160x90, 320x180 | list rows and cards, `srcset` 1x/2x |

Master in **lossless** WebP, derivatives in **lossy**. The rule inverts below
native resolution: on a native-resolution render lossless is 1.9x smaller than
q80, and on the downscaled `og` rung it is 2.9x **larger**, because resampling
turns crisp glyph edges into the gradients a DCT codec is good at.

There is no `avatar` rung. A square crop from the top of a render 816 CSS px
wide is an 8.5x downscale, which lands 16 px body text under 2 px tall: not a
picture of a realm, an illegible fragment of its first heading. Use an identicon
for a square identity mark; it is honestly synthetic rather than dishonestly
photographic.

## Tiles, for when there is no picture

Never a broken image and never a blank one. A blank box reads as "the site is
broken", not "this one has no render yet", and the reader this exists for does
not know the difference.

Six states, each with a short label for a dense table and a plain line for
everyone else. The plain line belongs in the `alt` at every size, because a
screen reader has no size.

| kind | label | plain line |
|---|---|---|
| `parked` | not enabled yet | Published, but still waiting for approval before anyone can use it. |
| `pure` | code library | Building blocks other programs use. It has no page of its own. |
| `no-render` | no page to show | This program works. It just does not display anything. |
| `directory` | folder | A folder, not a program. The things inside it have their own pages. |
| `none` | no preview | No picture available for this one. |
| `error` | unavailable | Could not load a picture of this one just now. |

The colour is seeded from `sha256(path)`, so a package gets the same tile on
every surface and across restarts, and the tile is cacheable forever.

## Caching

**The cache key is the sha256 of the gnoweb response body**, not the URL and not
a TTL. A realm's render is a function of chain state: it changes when nobody
redeploys anything, and it stays put through a hundred page views. The body hash
is exact, and it covers the `:path` arguments and the gnoweb build id for free.
The probe that computes it is one HTTP GET, ~45 ms, against ~500 ms for
`vm/qrender`, which would not see either of those anyway.

Serving is stale-while-revalidate: a cached picture goes out immediately and the
re-probe happens behind it. A reader never waits on Chrome.

A transient upstream error never overwrites a good picture. The realm was fine a
minute ago and will be again, and an error tile in place of a slightly old
screenshot is the worse answer.

## Safety

`?url=` is restricted to an allowlist (`-allow`, default `gno.land` and the
testnets). An unbounded one is an open proxy and a way to spend this box's CPU
on headless Chrome. `POST /invalidate` needs a bearer token and is refused
outright when none is configured.

## Cost

Chrome is the part everyone expects to be slow and it is not. Measured on the
deployment box, 2026-09-22, `/r/gov/dao` render only: **4.6 s end to end**, of
which the capture is 0.8 s and a browser launch is ~0.5 s and happens once per
worker. The rest is WebP.

That is why **the sweep captures the render and not the page**. The page master
is 2560x3254 against the render's 1632x2714, nothing derives a rung from it, and
the pixel count is the whole cost. A page master is captured when somebody
actually asks for one, and one already on disk survives a render-only refresh.

Two things that are not levers, both measured rather than assumed:

- **Encoder effort.** Methods 1 through 6 produce byte-identical output on the
  same image, within 0.93 to 1.23 s. Only method 0 differs, at 7.7x the bytes.
- **Lossy for the page master.** 261,280 bytes at q90 against 150,064 lossless
  on the same page. The rule only inverts below native resolution.

RAM is the constraint rather than CPU: roughly 1.3 GB per worker. Storage is
~300 KB per path with both masters, ~180 KB with the render alone.

Two workers on a box that already runs something else is the right size.

## CLI

```
gnoshot serve                                 run the service
gnoshot capture <url> [-out DIR] [-mode ...]  one URL, the full ladder, no service
gnoshot resolve <url>...                      which selector matches, and the box
gnoshot ladder  <master.png>                  derivatives only, no browser
gnoshot tile    <path> [-kind ...]            one placeholder
gnoshot sweep   [-service URL]                enqueue every known path
```

`capture` and the worker run the identical code path, so a picture that is wrong
in production is reproducible with one command and nothing running.
