# voetbal

> [!IMPORTANT]
> The app denies all access by default. You must set `VOETBAL_NETWORK_LOCK` or `VOETBAL_REGION_LOCK` (or both) before it will serve streams — see [Access locks](#access-locks) below. Until one of them is set, the homepage only shows a setup notice and every other request is rejected.

## Pull the image

```
docker pull ghcr.io/st3fan/voetbal:main
```

Images are available for `linux/amd64` and `linux/arm64`.

## Run in the foreground

```
docker run --rm -p 8000:8000 ghcr.io/st3fan/voetbal:main
```

The app listens on <http://localhost:8000>. Stop it with Ctrl-C.

## Run in the background

```
docker run -d --name voetbal --restart unless-stopped -p 8000:8000 \
  -v voetbal-data:/data \
  ghcr.io/st3fan/voetbal:main
```

With `--restart unless-stopped` the container comes back automatically after a crash or a reboot of the host, until you stop it yourself. The volume on `/data` persists the disk cache and the geo IP database across restarts — see [Caching](#caching).

Manage it with:

```
docker logs -f voetbal
docker stop voetbal
docker start voetbal
docker rm voetbal
```

## Access locks

Access is controlled by two locks: a region lock and a network lock. **At least one of them must be configured** — when neither `VOETBAL_REGION_LOCK` nor `VOETBAL_NETWORK_LOCK` is set, the app defaults to no access: the homepage only shows a notice telling you to set one of these options, and all other requests are denied. The `docker run` commands above therefore need one of the `-e` options from the sections below to be useful.

When both locks are configured, the network lock is checked first, and a request is allowed when either lock passes. Requests from private and loopback addresses are always allowed once at least one lock is configured.

### Region lock

Set `VOETBAL_REGION_LOCK` to a comma-separated list of two-letter country codes to only allow access from those countries. When enabled, the app downloads a geo IP database into its data directory on startup (and refreshes it weekly), so mount a volume there to persist it. The data directory defaults to `/data` and can be changed with `VOETBAL_DATA_PATH`:

```
docker run --rm -p 8000:8000 \
  -e VOETBAL_REGION_LOCK=CA,US \
  -v voetbal-data:/data \
  ghcr.io/st3fan/voetbal:main
```

IP geolocation by [DB-IP](https://db-ip.com) (CC BY 4.0).

### Network lock

Set `VOETBAL_NETWORK_LOCK` to a comma-separated list of networks to only allow access from those networks. Each entry can be:

- an IP address, e.g. `1.1.1.1` or `2001:db8::1` — allows exactly that address
- a CIDR prefix, e.g. `192.168.0.0/16` or `2001:db8::/32` — allows the whole range
- an ASN, e.g. `ASN577` or `AS577` (case-insensitive) — allows every prefix announced by that autonomous system; useful to allow your whole ISP

For ASN entries the announced prefixes are fetched once on startup from the [RIPEstat announced-prefixes API](https://stat.ripe.net/docs/data-api/api-endpoints/announced-prefixes); restart the container to pick up changes to the announced prefixes. If an entry cannot be parsed, or the prefixes for an ASN cannot be fetched, the app logs the error and exits instead of starting with a partial lock.

```
docker run --rm -p 8000:8000 \
  -e VOETBAL_NETWORK_LOCK=192.168.0.0/16,1.1.1.1,ASN577 \
  ghcr.io/st3fan/voetbal:main
```

## Watching streams

The homepage lists the live NOS streams with one entry per available quality:

- **Web:** links open the in-browser player at `/player/nos/{id}/{resolution}`.
- **URL:** buttons copy a short stream URL like `http://host:8000/stream/nos/2616266/1920x1080` for use in an external player (VLC, IPTV apps, a TV). The URL serves a standard HLS playlist; everything it references is proxied through the app, so the external player never talks to NOS directly.

Stream ids and resolutions are resolved against the NOS API on every request — a URL keeps working for as long as NOS broadcasts that stream, and returns 404 afterwards.

### IPTV playlist

`http://host:8000/playlist.m3u` serves an extended M3U playlist for IPTV applications: one channel per stream and quality (highest first, with name, logo and group attributes). Point an IPTV app at that URL and the channel list stays in sync with whatever NOS is broadcasting.

## Caching

Segments flow through a two-tier cache. Each tier is bounded by a TTL **and** a size — whichever limit is hit first starts evicting, oldest first:

- **Memory** (default: 3 minutes / 512MB) — concurrent viewers of the same stream share a single upstream fetch, and everyone watching live is served the same recent segments from memory. Configure with `VOETBAL_MEMORY_CACHE_TTL` and `VOETBAL_MEMORY_CACHE_SIZE`.
- **Disk** (default: 180 minutes / 12GB) — every fetched segment is also written to `$VOETBAL_DATA_PATH/cache`, so seeking back in a stream is served from disk instead of the CDN. NOS playlists carry roughly a 140-minute seek-back window, which the default TTL covers. The cache survives restarts when `/data` is on a volume. Configure with `VOETBAL_DISK_CACHE_TTL` and `VOETBAL_DISK_CACHE_SIZE`; set the size to `0` to disable the disk tier.

TTLs use Go duration syntax (`90s`, `3m`, `12h`); sizes accept `512MB`, `12GB`, or a plain number of megabytes.

The segment caches are still experimental. They are enabled by default, but each tier can be turned off independently with `VOETBAL_MEMORY_CACHE_DISABLED=1` or `VOETBAL_DISK_CACHE_DISABLED=1` (`true` also works; unset or `0`/`false` keeps the tier on). With the memory tier off, every request fetches straight from the CDN and concurrent viewers no longer share fetches; with the disk tier off, nothing is written to `$VOETBAL_DATA_PATH/cache` and seeking back is served by the CDN.

```
docker run -d --name voetbal --restart unless-stopped -p 8000:8000 \
  -e VOETBAL_NETWORK_LOCK=192.168.0.0/16 \
  -e VOETBAL_MEMORY_CACHE_SIZE=150MB \
  -e VOETBAL_DISK_CACHE_SIZE=4GB \
  -v voetbal-data:/data \
  ghcr.io/st3fan/voetbal:main
```

Rough sizing: cache bytes ≈ bitrate ÷ 8 × retention, per variant actually being watched. At NOS bitrates that is about 0.55 MB/s for 1080p (≈ 500MB per 15 minutes) down to 0.11 MB/s for 360p. Only requested segments are cached, so idle streams cost nothing. On small hosts, keep `VOETBAL_MEMORY_CACHE_SIZE` well under available RAM and consider setting `GOMEMLIMIT` (e.g. `GOMEMLIMIT=230MiB`) so the Go runtime stays inside your memory budget.

Stream metadata (the NOS stream list and the per-stream playlist locations) is cached in memory for 12 hours and re-fetched on demand; this is not configurable.

## Status pages

- `/watchers` shows who is currently watching (IP addresses anonymized to their first two octets), since when, and how many requests they made.
- `/caches` shows every cache entry with its expiration and contents, hit/miss statistics per tier, and how full each tier is.

Both pages sit behind the access locks, auto-refresh every 5 seconds, and are linked for humans rather than machines — there is no JSON API.

## Logging

All logging goes to **stdout** as one JSON object per line, mostly at level `INFO`. Four kinds of entries:

- application events (startup configuration, cache setup, lock configuration),
- one entry per incoming HTTP request — method, path, status, latency and a `client_ip` masked to its first two octets; request/response bodies and headers are never logged,
- one entry per outgoing HTTP request to NOS/CDN — method, full URL, status and duration,
- `WARN` entries flagging stutter risk when a segment is slow: `slow upstream segment` (a shared upstream download took longer than `VOETBAL_SLOW_SEGMENT_WARN`) and `slow segment delivery` (a viewer's time-to-first-byte exceeded it, tagged with the serving `tier`).

`docker logs -f voetbal` streams them; pipe through `jq` to filter, e.g. `docker logs voetbal | jq 'select(.msg == "upstream request")'`. To watch for stutter, tail the warnings: `docker logs -f voetbal | jq 'select(.level == "WARN")'`.

## Short copy URLs (deprecated)

`VOETBAL_COPY_SHORT_URLS` predates the short `/stream/nos/…` URLs and no longer affects the homepage — the copy buttons always hand out short URLs now. Previously copied `/r/{code}` links keep redirecting to the stream they were created for. The option can be removed from your configuration.

## Configuration reference

| Variable | Default | Purpose |
|---|---|---|
| `VOETBAL_NETWORK_LOCK` | *(unset)* | allow access from IPs / CIDRs / ASNs; unset together with region lock = deny all |
| `VOETBAL_REGION_LOCK` | *(unset)* | allow access from countries (two-letter codes) |
| `VOETBAL_DATA_PATH` | `/data` | directory for the geo IP database and the disk cache |
| `VOETBAL_MEMORY_CACHE_TTL` | `3m` | how long segments stay in memory |
| `VOETBAL_MEMORY_CACHE_SIZE` | `512MB` | memory cache cap (`0` is not meaningful; lower it instead) |
| `VOETBAL_MEMORY_CACHE_DISABLED` | *(unset)* | `1` or `true` turns off the memory tier |
| `VOETBAL_DISK_CACHE_TTL` | `3h` | how long segments stay on disk |
| `VOETBAL_DISK_CACHE_SIZE` | `12GB` | disk cache cap; `0` disables the disk tier |
| `VOETBAL_DISK_CACHE_DISABLED` | *(unset)* | `1` or `true` turns off the disk tier |
| `VOETBAL_SLOW_SEGMENT_WARN` | `3s` | log a `WARN` when an upstream segment fetch, or a viewer's time-to-first-byte, exceeds this |
| `VOETBAL_COPY_SHORT_URLS` | *(unset)* | deprecated, no longer used |

## Update

```
docker pull ghcr.io/st3fan/voetbal:main
docker stop voetbal
docker rm voetbal
docker run -d --name voetbal --restart unless-stopped -p 8000:8000 \
  -v voetbal-data:/data \
  ghcr.io/st3fan/voetbal:main
```
