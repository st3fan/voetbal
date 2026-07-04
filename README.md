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
docker run -d --name voetbal --restart unless-stopped -p 8000:8000 ghcr.io/st3fan/voetbal:main
```

With `--restart unless-stopped` the container comes back automatically after a crash or a reboot of the host, until you stop it yourself.

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

Set `VOETBAL_REGION_LOCK` to a comma-separated list of two-letter country codes to only allow access from those countries. When enabled, the app downloads a geo IP database into `/data` on startup (and refreshes it weekly), so mount a volume there to persist it:

```
docker run --rm -p 8000:8000 \
  -e VOETBAL_REGION_LOCK=CA,US \
  -v voetbal-data:/data \
  ghcr.io/st3fan/voetbal:main
```

```
docker run -d --name voetbal --restart unless-stopped -p 8000:8000 \
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

## Short copy URLs

The "URL:" buttons on the homepage copy a stream URL for use in an external player. By default that is the full proxied URL (`http://host:8000/proxy?url=...`), which is painful to type into a TV or phone by hand. Set `VOETBAL_COPY_SHORT_URLS` to any non-empty value to copy a short URL instead, like `http://host:8000/r/a3f`, which redirects to the same proxied stream:

```
docker run -d --name voetbal --restart unless-stopped -p 8000:8000 \
  -e VOETBAL_NETWORK_LOCK=192.168.0.0/16 \
  -e VOETBAL_COPY_SHORT_URLS=1 \
  ghcr.io/st3fan/voetbal:main
```

The code is derived from a hash of the stream URL (normally 3 lowercase hex characters, one more per collision), so a short URL keeps working across restarts for as long as NOS serves the stream under the same URL.

## Update

```
docker pull ghcr.io/st3fan/voetbal:main
docker stop voetbal
docker rm voetbal
docker run -d --name voetbal --restart unless-stopped -p 8000:8000 ghcr.io/st3fan/voetbal:main
```
