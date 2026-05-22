# Lantern brand assets

The Lantern brand mark is a paper lantern in Filecoin blue. Portable light: the chain in your hand, not in a data center.

## Files

| File | Purpose |
|------|---------|
| `lantern-mark.svg` | Master mark, 64×84 viewBox. Use this for any rendering. |
| `lantern-mark-{32,64,128,256,512,1024}.png` | Rasterized renders for legacy embedding (Markdown, GitHub README, app icons). |
| `lantern-wordmark.svg` | Master wordmark, mark + "Lantern" text. |
| `lantern-wordmark.png` | 800×200 raster. |
| `lantern-favicon.svg` | Square-viewBox variant for browser favicons. |
| `lantern-favicon-{32,64}.png` | Raster favicons for environments that don't render SVG. |

## Palette

- **Primary blue** `#0090ff` (Filecoin blue)
- **Wordmark navy** `#0a1628`
- **Mark stroke (lockup)** `#0066cc` (slightly darker than primary for visual weight parity with the wordmark)
- **Body fill** `rgba(0, 144, 255, 0.14)` translucent

These align with the operator dashboard (`cmd/lantern/dashboard/index.html`) and with `calix-mainnet.reiers.io` for visual consistency across the reiers.io Filecoin tools.

## Re-rendering PNGs from the SVG masters

Requires `rsvg-convert` from `librsvg`:

```sh
brew install librsvg                    # macOS
apt-get install -y librsvg2-bin         # Debian/Ubuntu

cd docs/assets

# Mark, multiple sizes
for size in 32 64 128 256 512 1024; do
  rsvg-convert -w "$size" lantern-mark.svg -o "lantern-mark-${size}.png"
done

# Favicons
rsvg-convert -w 32 -h 32 lantern-favicon.svg -o lantern-favicon-32.png
rsvg-convert -w 64 -h 64 lantern-favicon.svg -o lantern-favicon-64.png

# Wordmark
rsvg-convert -w 800 lantern-wordmark.svg -o lantern-wordmark.png
```

The SVG masters are the source of truth. Don't edit the PNGs by hand; re-render them.

## Where the mark is used today

- `README.md` hero
- `cmd/lantern/dashboard/index.html` header (served live from the embedded `lantern-mark.svg`)
- Browser favicon for the dashboard (served from embedded `lantern-favicon.svg`)

## What's intentionally NOT in this directory

- `lantern-logo.png` (1024×1024 mood-piece illustration) is kept for now but should be considered legacy. The README and all in-product surfaces have moved to the proper brand mark.
