# Marketing Site Handoff

You are picking up work on two public-facing sites managed by `arbeithand` (the handarbeit-admin GitHub account). Both serve from GitHub Pages, both use Cloudflare DNS, and the domain is registered at Squarespace.

## Sites

| URL | Repo | Local path | Source |
|---|---|---|---|
| https://handarbeit.io | `handarbeit/handarbeit.io` (private) | `/home/user/dev/handarbeit.io` | `main /` |
| https://fabrik.handarbeit.io | `handarbeit/fabrik` | `/home/user/dev/fabrik` | `main /docs` |
| https://www.handarbeit.io | (same as apex) | — | — |
| https://fabrik.handarbeit.dev | `handarbeit/fabrik-dev-redirect` (public) | — | HTTP redirect to fabrik.handarbeit.io |

Both Pages sites have **HTTPS enforced** and **Let's Encrypt certs** auto-renewing.

## DNS (Cloudflare)

- **Nameservers:** `cruz.ns.cloudflare.com`, `finley.ns.cloudflare.com`
- **Apex A** (`handarbeit.io`): `185.199.108.153`, `.109.153`, `.110.153`, `.111.153` (GitHub Pages)
- **CNAME `www`** → `handarbeit.github.io`
- **CNAME `fabrik`** → `handarbeit.github.io`
- **MX `@`** → `smtp.google.com` (Google Workspace)
- **All records must be DNS-only (gray cloud)** — never proxy through Cloudflare, it breaks GitHub Pages cert provisioning.
- Domain registration stays at Squarespace ($70/yr, renews May 5). Squarespace no longer controls DNS.
- WHOIS is anonymized — only `MA, US` visible (registry-required). Don't add an Organization back.

## Brand

- **Accent color:** `#ea782e` (orange, sampled from the emblem icons). Defined in `docs/assets/css/style.scss` as `$color-accent`.
- **handarbeit.io tagline:** "Hand-crafted software"
- **fabrik tagline:** "Your SDLC, on autopilot"
- **Emblem:** `/home/user/dev/handarbeit.io/assets/handarbeit.png` — transparent background, must keep transparent. The handarbeit.io landing page emblem links to `https://fabrik.handarbeit.io`.
- **Hero video:** `docs/assets/videos/fabrik-demo.mp4` (3.4 MB). Embedded with `autoplay muted playsinline controls`, **no loop**.

## Commit identity — CRITICAL

Always commit as `arbeithand`, never as the default `gh` CLI user (`arbeithand`):

```bash
git -c user.email=handarbeit@handarbeit.io -c user.name=arbeithand commit -m "..."
```

Verify with `git log --pretty=format:'%h %an <%ae>' -1` after each commit.

## Auth

- Token: `FABRIK_TOKEN` in `/home/user/dev/fabrik/.env` (a classic PAT for the `arbeithand` account, scopes: `admin:org, project, repo, workflow, write:discussion`)
- For `gh` CLI: `GH_TOKEN=$(grep '^FABRIK_TOKEN=' /home/user/dev/fabrik/.env | cut -d= -f2-) gh ...`
- For pushes to `handarbeit/handarbeit.io`: HTTPS remote works; the credential helper picks up `$FABRIK_TOKEN` when injected. Pushes to `handarbeit/fabrik` use the user's existing SSH/credential setup.
- **Do not run `gh auth login`** — that switches the active account.

## Deploy cycle

1. Edit → commit (with arbeithand identity) → `git push origin main`
2. GitHub Pages rebuilds in ~40 s.
3. Verify:
   ```bash
   GH_TOKEN=$(grep '^FABRIK_TOKEN=' /home/user/dev/fabrik/.env | cut -d= -f2-) \
     gh api repos/handarbeit/<repo>/pages --jq '{status, https_enforced, html_url}'
   curl -sI https://handarbeit.io | head -3
   ```

## Repo-specific constraints (fabrik)

The fabrik repo has a `CLAUDE.md` with engineering rules. Two that matter for site work:

- If you edit `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, or `docs/positioning.md`, you MUST regenerate `docs/llms-full.txt` in the same commit: `bash scripts/generate-llms-full.sh && git add docs/llms-full.txt`. Editing `docs/index.md` or CSS does NOT trigger this.
- Don't commit to `main` from a worktree. (You're working in the user's main checkout, so this shouldn't come up.)

## Outstanding TODOs

- **Make pipeline-stage tiles clickable.** The 6 stage tiles in "How It Works" (`docs/index.md` line ~88) should become anchored links to per-stage detail sections lower on the page (`#specify`, `#research`, etc.). User picked the "anchor to dedicated section below" option over inline expand / modal — but the detail sections themselves were never written. Pull content from `docs/stage-lifecycle.md` and `docs/state-machine.md`.

## Don't touch / gotchas

- **Squarespace site connection** — domain was bound to a trial Squarespace site on a different account that force-injected apex IPs and silently overrode custom DNS. Resolved by parking + moving DNS to Cloudflare. Don't reconnect a site to this domain.
- **`fabrik-dev-redirect` repo** — separate static redirect; leave alone.
- **Repo visibility for `handarbeit/fabrik`** — the install instructions use `go install github.com/handarbeit/fabrik@latest`, which only works if the repo is public. Check `gh api repos/handarbeit/fabrik --jq .visibility` before claiming the docs are accurate for a stranger reader.

## Verification quick reference

```bash
# Apex DNS (Cloudflare authoritative)
dig +short handarbeit.io A @cruz.ns.cloudflare.com
# Pages build state
GH_TOKEN=$(grep '^FABRIK_TOKEN=' /home/user/dev/fabrik/.env | cut -d= -f2-) \
  gh api repos/handarbeit/handarbeit.io/pages
# Certificate
echo | openssl s_client -connect handarbeit.io:443 -servername handarbeit.io 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates
# Most recent commits on each site
git -C /home/user/dev/handarbeit.io log --oneline -5
git -C /home/user/dev/fabrik log --oneline -- docs/ | head -5
```
