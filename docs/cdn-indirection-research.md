<!--
SPDX-FileCopyrightText: 2026 Slavi Pantaleev

SPDX-License-Identifier: AGPL-3.0-or-later
-->

# CDN indirection ("fronting") research

Status: research note, 2026-10-27. No code changes. Question: *can we hide the CDN origin
domain/IP from link recipients (and optionally protect the origin) with a free front layer -
Cloudflare Worker, Cloudflare Tunnel, a second domain, another service - flexibly mixed?* The
judgment call was deliberately left at lower priority.

Short answer: **yes, cheaply, but it buys cosmetic privacy at a real trust cost.** The smallest
useful form is a neutral domain in front of the existing CDN host (Cloudflare Worker; add a
Tunnel if the origin must have no public surface). Do not cache `/f/*` at the edge, keep
uploads well under 100 MB, and treat any TLS-terminating front as a party that can tamper with
the wrapper page (never with file contents). Verdict: optional, documented, off by default; no
per-user front domains.

Assumptions: everything below is based on `docs/cdn-file-service.md` and `connector/cdn.go`
(link `https://cdn.example.com/f/<id>#<key>`, zero-knowledge AES-256-GCM, `no-store` wrapper,
`private` ciphertext, bearer upload/list/revoke, expiry enforced on read + hourly GC) and on the
linked provider documentation as of 2026-10-27. No Cloudflare account was used to test the
Worker recipe; it is a reviewed sketch, not a deployment.

## 1. What is exposed today

- The link text shows `cdn.<homeserver-domain>` to the recipient. For SMS/MMS recipients that
  is the **only** automatic leak of the Matrix/homeserver identity - they see a phone number,
  never the room or MXIDs.
- DNS for the CDN hostname reveals the origin IP; the TLS certificate names the same host.
- The wrapper fetches `/f/<id>/raw` relative to the same origin (no origin URL in the HTML),
  already sends `Referrer-Policy: no-referrer` and a sandboxed CSP; `/raw` is
  `application/octet-stream`, `nosniff`, `Cache-Control: private`.

Who sees what:

| Observer | Sees today | Can a front change it? |
|---|---|---|
| Recipient | Link hostname, origin IP via DNS | Yes - neutral hostname; IP hidden if the front proxies |
| Recipient's link scanners/unfurlers | Wrapper page, file id | Yes - they hit the front, not the origin |
| Origin-side scanners / DDoS | Origin host, ports | Partially; a Tunnel removes inbound entirely |
| Front provider (whoever fronts) | Recipient IPs, ids, HTML, ciphertext | No - this is the price; the front sees this instead of the origin alone |
| Origin's host/VPS provider | Every CDN connection | Tunnel reduces it to one outbound connector |
| Sender's carrier | The link text in the SMS body | No |

"Hide from recipients" means: do not hand them the origin host/IP. It cannot mean hiding that a
link exists or hiding its visible domain.

## 2. What any front can and cannot do

- **Cannot**: hide the visible domain from the recipient (it is the URL), hide the link from the
  carrier, or decrypt anything (the key is in the `#fragment` and is never sent).
- **Can**: make the visible host neutral; absorb scanners/DDoS at an edge; hide the origin IP
  (Tunnel fully; Worker if the origin hostname stays unpublished); move metadata exposure from
  the origin's host provider to the front provider.
- **Trust cost (the important one)**: every TLS-terminating front - Worker, Tunnel, Deno, a
  normal VPS reverse proxy - can rewrite the wrapper page. That page *is* the decryption
  software; the fragment key stays local until the recipient's browser runs the page, and a
  tampered page can exfiltrate `location.hash`. The CSP does not help: the front controls the
  response headers too. Only a TLS/SNI **passthrough** proxy (a box you run) is cryptographically
  unable to do this. File contents are never at risk: fronts see ciphertext only.
- Fronting does not make the link disappear from message metadata elsewhere: it is in the SMS/MMS
  body the carrier already sees.

## 3. Option comparison

| Option | Recipient sees | Hides origin IP? | Origin protected? | Config needed | Free tier / cost | Latency | Revocation/expiry | Wrapper tamper risk |
|---|---|---|---|---|---|---|---|---|
| Worker proxy + neutral domain | Neutral hostname | From recipients yes (origin host unpublished); resolvers of the origin host can still find it | Edge DDoS/WAF-ish; origin still has inbound | Domain DNS on Cloudflare, Worker + custom domain, bridge advertises front host | 100k req/day, 10 ms CPU; request body ≤ 100 MB; no egress fees ([limits](https://developers.cloudflare.com/workers/platform/limits/), [pricing](https://developers.cloudflare.com/workers/platform/pricing/)) | +~10-50 ms TTFB; no caching, so bytes cross front↔origin twice | Transparent if `/f/*` is never cached | Yes (CF) |
| Tunnel (`cloudflared`) | Neutral hostname | Fully (no inbound, no resolvable origin) | No inbound; CF edge protections | `cloudflared`, CNAME to `<uuid>.cfargotunnel.com`, ingress to bridge port | Free on all plans; 1,000 tunnels/account ([routing](https://developers.cloudflare.com/tunnel/concepts/routing/)) | One extra hop; connector keeps near-CF latency | Transparent; watch proxy timeouts | Yes (CF) |
| Plain second domain (A record to origin) | Second hostname | No - `dig` exposes the IP | No | A/AAAA record + cert (Traefik certResolver) | Domain cost only | Direct | Transparent | No (self) |
| Own VPS, TLS/SNI passthrough | Neutral hostname | Yes | VPS absorbs traffic; no WAF | VPS + `stream`/`ssl_preread` + cert (DNS-01) on origin | Oracle Always Free (2 AMD micro, ~2 ARM OCPU/12 GB, 10 TB/mo egress; capacity flaky) or ~$4/mo VPS ([Oracle](https://docs.oracle.com/en-us/iaas/Content/FreeTier/freetier_topic-Always_Free_Resources.htm)) | VPS hop | Transparent | **No** (front sees ciphertext only) |
| Own VPS, TLS termination | Neutral hostname | Yes | VPS absorbs traffic | VPS + nginx/Caddy + cert on VPS | Same | VPS hop | Transparent | Yes (your VPS) |
| Deno Deploy proxy | Neutral hostname | Yes | Deno edge, no WAF ads | Deno app + custom domain | 1M req/mo, 20 GiB egress/mo, 5 custom domains, no wildcard free ([pricing](https://deno.com/deploy/pricing)) | 2 regions on new Deploy | Transparent | Yes (Deno) |
| ngrok free | ngrok-assigned host + interstitial | Yes | ngrok | agent + endpoint | $5 one-time credit, 1 GB, 20k req., interstitial page ([pricing](https://ngrok.com/pricing)) | agent hop | Interstitial breaks wrapper UX | Yes (ngrok) - **unsuitable** |
| Tor onion / IPFS gateway | `.onion`; gateway host | Yes | n/a | onion service / pinning | Free | Tor slow; gateway varies | Onion yes; IPFS has no revoke/expiry - **rejects our semantics** | varies |
| Vercel/Netlify functions | Neutral hostname | Yes | platform | function proxy | Hobby tiers are a poor fit (function payload limits, hobby ToS) - not evaluated further | - | - | Yes |
| R2 + Worker (no origin at all) | Neutral hostname | Yes | CF | Worker + bucket, bridge storage backend change | 10 GB store, 1M Class A / 10M Class B ops, zero egress ([pricing](https://developers.cloudflare.com/workers/platform/pricing/)) | CF-internal | Delete object; don't cache | Yes (CF); bigger code change |

Two footnotes that apply to all Cloudflare fronting: public hostname routes (including Tunnel) on
Free/Pro/Business are subject to the CDN terms - "video or a disproportionate percentage of
pictures, audio files, or other large files" must use a paid service such as the Developer
Platform (Workers Paid), Images or Stream
([terms](https://www.cloudflare.com/service-specific-terms-application-services/#content-delivery-network-free-pro-or-business),
[policy](https://developers.cloudflare.com/fundamentals/reference/policies-compliances/delivering-videos-with-cloudflare/)).
And proxied paths have a 125 s read / 30 s write timeout (not configurable) that can 524 on slow
large uploads ([connection limits](https://developers.cloudflare.com/fundamentals/reference/connection-limits/),
[error 524](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-524/)).

## 4. Notes per option

### Cloudflare Worker proxy (recommended front)

- Flow: client → `files.example.net` (Worker) → `https://cdn-f7a2.example.com` (Traefik) →
  bridge `:29322`. The Worker is a byte-for-byte pass-through of `/f*`; the link the recipient
  opens never names the origin.
- Workers cannot override the `Host` header
  ([community](https://community.cloudflare.com/t/not-possible-to-override-the-host-header-on-workers-requests/13077)),
  so the Worker must fetch the origin hostname; no Traefik change is needed for that leg. To make
  *links* use the front domain, the bridge has to advertise it. **Playbook gap:** `cdn_domain`
  currently feeds both the Traefik `Host()` rule and the bridge's advertised base URL
  (`simple-config.yaml.j2` `cdn.domain`, `connector/cdn.go` `BaseURL()`). Split it:
  `cdn_public_domain` (bridge, advertised) vs `cdn_origin_domain` (Traefik rule, DNS target).
  Until that exists, an app-side "share host" override achieves the same with no bridge change.
- Limits that matter: 100,000 requests/day, 10 ms CPU (a proxy is I/O bound); request bodies are
  capped at **100 MB by account plan (Free/Pro)**, so with a front set
  `matrix_bridge_mautrix_android_sms_mms_cdn_max_file_bytes` to ≤ 95 MiB - the 100 MiB default
  would 413 at Cloudflare. Responses stream with no enforced limit. No egress charges. Custom
  domains are free (100/zone) and first-level hostnames get free Universal SSL
  ([Universal SSL](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/)).
- Cache: Workers responses are not automatically cached and the origin's `private`/`no-store`
  headers are respected, so nothing needs to change; optionally add a "Bypass cache" Cache Rule
  for `*/f/*` to make that explicit.
- Zone caution: disable HTML rewriting/optimization features for the front zone (Bot Fight Mode,
  Rocket Loader, email obfuscation, anything that touches HTML) so the sandboxed wrapper page and
  its inline CSP survive untouched; keep Always Use HTTPS on.
- Failure mode: exceeding the daily request limit returns Cloudflare 1027; routes can fail open
  (bypass) or closed. A personal bridge is nowhere near 100k requests/day.
- Score: best balance for the stated goal. Free, no new infrastructure, hides the origin host
  from recipients, keeps revocation semantics.

Worker sketch (ES module; origin hostname only in the script):

```js
// Worker for a neutral CDN front domain. Everything under /f is proxied verbatim to the
// bridge's CDN hostname; nothing is cached or rewritten.
const ORIGIN = "https://cdn-f7a2.example.com"; // never published to recipients

export default {
  async fetch(request) {
    const url = new URL(request.url);
    if (!url.pathname.startsWith("/f")) {
      return new Response("Not found", { status: 404 });
    }
    const target = new URL(url.pathname + url.search, ORIGIN);
    const response = await fetch(
      new Request(target, {
        method: request.method,
        headers: request.headers, // keeps Authorization, Accept-Encoding, ...
        body: request.body,       // streamed; null on GET/DELETE
        redirect: "manual",
      }),
      { cache: "no-store" }, // keep Cloudflare's caches out of the loop
    );
    const headers = new Headers(response.headers);
    headers.set("Referrer-Policy", "no-referrer"); // defense in depth; origin already sets it
    return new Response(response.body, {
      status: response.status,
      statusText: response.statusText,
      headers,
    });
  },
};
```

Wrangler route sketch:

```toml
name = "sms-cdn-front"
main = "src/worker.js"
compatibility_date = "2026-10-01"

[[routes]]
pattern = "files.example.net"
custom_domain = true
```

### Cloudflare Tunnel

- `cloudflared` dials out; Cloudflare publishes the hostname via a CNAME to
  `<UUID>.cfargotunnel.com`. No inbound port, no resolvable origin, no origin certificate
  needed if the service is `http://127.0.0.1:<host-bound-29322>`.
- Restrict ingress to the CDN path, otherwise the neutral hostname also exposes `/_smsmms/*`
  (the bridge serves both on the same listener):

```yaml
ingress:
  - hostname: files.example.net
    path: ^/f(/.*)?$
    service: http://127.0.0.1:29322
  - service: http_status:404
```

- The bridge does not care about the Host header; set `cdn.domain: files.example.net` so links
  and uploads use the front. The playbook's Traefik CDN router becomes optional for this path.
- Same Cloudflare terms caveat as the Worker; proxied requests keep the 125 s read / 30 s write
  timeouts, so cap uploads (≤ 50 MiB recommended) or expect 524 on slow mobile links.
- Score: the best "protect the origin" option (nothing inbound at all) and simpler than
  Worker+domain, but slightly riskier for big uploads and it keeps Cloudflare in the path anyway.

### Plain second domain

- A/AAAA straight at the origin hides only the *name*: anyone can `dig` the shared hostname and
  see the IP. Worth it only as the minimal baseline or as the link-host layer if the user insists
  on a domain with no third party; costs a domain, no code.

### Own VPS, two modes

- **TLS passthrough** (`stream` + `ssl_preread`, forwarding raw TCP): the only front that cannot
  read or tamper with the wrapper page. The neutral domain's certificate must be issued to the
  origin (DNS-01), because the VPS never sees plaintext. Hides the origin IP from recipients and
  from the VPS's own provider is less clear (the VPS sees connections). No WAF: it is a dumb,
  attack-absorbing relay.
- **TLS termination**: behaves like Cloudflare in terms of tamper ability, but you own it.
- Free tier possible (Oracle Always Free; ARM capacity is flaky) or a small VPS; more moving parts
  than any Cloudflare option.

### Deno Deploy / ngrok / Tor / IPFS / serverless

- Deno: simplest non-Cloudflare Worker; free 1M req and 20 GiB egress/month is enough for a
  personal bridge, 5 custom domains, no free wildcard. Same trust caveat.
- ngrok: free plan shows an interstitial page and uses an ngrok-branded domain - it breaks the
  wrapper UX and is not neutral. Not viable.
- Tor onion: hides everything, but the link is not a normal HTTPS domain and recipients need Tor.
- IPFS gateways: no revoke and no expiry - incompatible with the service's core semantics.
- Vercel/Netlify function proxies: function response-body limits and hobby-tier terms make them a
  poor fit for 100 MB files; not pursued.
- R2 + Worker: not fronting but origin elimination; free 10 GB, no egress fees. Revisit if local
  disk storage becomes the problem, not the domain.

## 5. Expiry, revocation and caching (pass-through checklist)

- Proxy everything: `POST /f` (streamed body), `GET /f`, `GET /f/{id}`, `GET /f/{id}/raw`,
  `DELETE /f/{id}`, with the bearer `Authorization` header and the `expires` query string intact.
  Never rewrite method or status; revoked/expired must stay 404.
- Do not cache at the front. Cloudflare does not cache `private`, `no-store`, `no-cache`, or
  `max-age=0` responses, and Free/Pro/Business have origin cache-control enabled by default
  ([default cache behavior](https://developers.cloudflare.com/cache/concepts/default-cache-behavior/)).
  `/f/*` has no cacheable file extension either. Just add no Cache Rules, or add an explicit
  "Bypass cache" rule for `*/f/*`. Without that, the only edge cache entry is the default 3-minute
  TTL for 404s, which is harmless (ids are random and never reused) but avoidable by making 404s
  `no-store` if deterministic behavior is wanted.
- The wrapper already fetches with `cache: "no-store"`, so a revoked link cannot come out of the
  browser cache either.
- If a front is ever allowed to cache ciphertext to save bandwidth, revocation and expiry stop
  being immediate until the edge TTL expires; a revoke-triggered purge would have to be added.
  Don't do it now.
- Size/time budget with a CF front: keep `max_file_bytes` ≤ 95 MiB for the 100 MB plan body cap,
  and ≤ ~50 MiB for reliability on slow mobile uploads (125 s read / 30 s write timeouts). The
  Worker leg itself (client → CF edge → Worker) is not subject to the origin leg's timeouts, but
  the Worker's fetch to the origin should be treated as a normal HTTP request over the internet.
- Hardening that is useful regardless of fronting: the Traefik CDN router currently matches on
  `Host()` only, so `cdn.example.com` also exposes `/_smsmms/*`; add `PathPrefix(`/f`)` (and the
  tunnel ingress path filter above) so the front host serves only the file service.

## 6. E2E safety

- **Contents**: AES-256-GCM with the key only in the fragment. Any front sees ciphertext, ids,
  sizes and timing; it cannot decrypt, forge or replay plaintext. The E2E property holds.
- **Key delivery**: a TLS-terminating front can serve a modified wrapper page and capture the
  fragment key from recipients who open links in a browser. That is a real, if narrow, downgrade:
  it only affects links clicked while the front is malicious or compromised. Mitigations, in
  order: (1) view links in the app, which fetches `/raw` and decrypts locally without loading any
  page script; (2) use a self-hosted TLS-passthrough front; (3) accept the front as a trust
  anchor. Document the choice where the front is configured.
- **Metadata**: the front learns recipient IPs, times, ids and sizes for every fetch; Cloudflare
  keeps logs. That is more than the origin reveals today, shifted to a third party.

## 7. Per-user custom front domain

- Today `cdn_base_url` is bridge-global: one value advertised in `GET /_smsmms/v1/info` and the
  pair-complete response. Per-user hosts need either shared wildcard subdomains
  (`alice.files.example.net`) or bring-your-own domains.
- Shared wildcard: free Universal SSL covers only the apex and first-level subdomains; deeper
  per-user names need Total TLS or advanced certificates (paid). Deno's free tier has no
  wildcards at all.
- BYO domains: Cloudflare for SaaS/Platform custom hostnames include 100 free, then
  $0.10/hostname ([plans](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/plans/));
  each user must CNAME and validate a domain, and the bridge/app must learn which host to build
  links with.
- The app already receives the full link from the bridge; an app-side "share host" override
  (upload to `cdn_base_url`, rewrite the visible host) is the lightest path, but it still needs a
  per-user setting and a per-user mapping on the front.
- Verdict: defer. An operator-level neutral domain already meets the goal (recipients do not see
  the homeserver); per-user domains only reduce cross-user correlation and add certificate,
  onboarding and support work.

## 8. Recommended recipe (smallest useful form)

Worker proxy on a neutral domain, origin hostname unpublished, nothing else.

1. Prerequisite: a neutral domain the operator owns; delegate its DNS to Cloudflare (full setup -
   Workers requires the zone on Cloudflare). Free.
2. Pick an origin hostname that is never shared, e.g. `cdn-f7a2.example.com`, and point it at the
   origin. Keep the existing Traefik router and certificate on it.
3. Cap uploads: `matrix_bridge_mautrix_android_sms_mms_cdn_max_file_bytes: 99614720` (95 MiB)
   or lower while a Cloudflare front is in the path.
4. Create the Worker (script in §4) and attach it to a custom domain on the neutral zone, e.g.
   `files.example.net`; free Universal SSL covers first-level hostnames.
5. Bridge-side wiring: advertise the front host (`cdn.domain` / `cdn_base_url` =
   `https://files.example.net`) while the Traefik `Host()` rule and the Worker's `ORIGIN` keep
   using `cdn-f7a2.example.com`. This needs the `cdn_public_domain` / `cdn_origin_domain` split
   described in §4; until then, use an app-side share-host override.
6. Zone settings: Always Use HTTPS on; Bot Fight Mode and HTML-rewriting features off; no cache
   rules added (optionally one "Bypass cache" rule for `*/f/*`).
7. Verify with curl: wrapper response has `Referrer-Policy: no-referrer` and
   `Cache-Control: no-store`, `CF-Cache-Status` is `DYNAMIC` or `BYPASS`; `/raw` is `private`;
   `POST`/`GET`/`DELETE` with a device token work; expired and revoked ids return 404; an upload
   above the cap returns the bridge's 413 before Cloudflare's 413 ever triggers.
8. Later, optionally: put `cdn-f7a2.example.com` behind a Cloudflare Tunnel so the origin has no
   inbound at all, or swap the Worker for a self-hosted TLS-passthrough relay if Cloudflare in
   the path is unacceptable.

## 9. Verdict

- **Worth it?** As an optional, documented deployment: yes, in the smallest form above. The
  concrete value is that SMS/MMS recipients no longer learn the homeserver domain (today the only
  place they see it) and the origin picks up free edge DDoS/scanner absorption.
- **Not worth it**: per-user domains; stacking Worker + Tunnel + VPS for a personal bridge; edge
  caching; treating the front as anything more than a convenience layer.
- **Costs to accept**: a third party sees recipient metadata and could tamper with the wrapper
  page (key-theft risk for browser viewers only); Cloudflare's terms expect a paid service for
  large-file/video delivery on free plans; a 100 MB request-body cap; one more failure mode
  (1027/524); one playbook variable split; docs.
- **Priority**: after the CDN itself ships. No bridge code changes are required; one role change
  plus documentation. Keep the feature off by default and the recipe opt-in.
- **If origin-IP protection is the primary goal** and a third party in the path is acceptable,
  Cloudflare Tunnel is the most value per effort; if no TLS-terminating third party is
  acceptable at all, a self-hosted SNI-passthrough VPS is the only option, and it costs more
  maintenance than the rest of this feature deserves.
