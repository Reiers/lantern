# get.lantern.reiers.io — Cloudflare Pages source

Serves `install.sh` at `https://get.lantern.reiers.io`. The one-line
installer (`curl -fsSL https://get.lantern.reiers.io | bash`) reads
from here.

## Files
- `install.sh` — copy of repo-root `install.sh`. Keep in sync.
- `_redirects` — `/` 200-rewrites to `/install.sh`.
- `_headers` — forces `text/plain; charset=utf-8` so browsers render
  the script as text instead of trying to execute it as HTML.

## Re-deploy after editing install.sh
```bash
cd deploy/get
cp ../../install.sh install.sh
wrangler pages deploy . --project-name=lantern-get --branch=main
```

## DNS / project layout
- Cloudflare Pages project: `lantern-get`
- Custom domain: `get.lantern.reiers.io` (CNAME → `lantern-get.pages.dev`, proxied)
- TLS cert: issued by Google CA via Cloudflare Pages

## History
- 2026-05-22 12:15 CPH: initial Pages deployment. Replaces previous
  ad-hoc Hetzner nginx setup (which was working but is now redundant).
