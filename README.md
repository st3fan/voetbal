# voetbal

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

## Region lock (optional)

Set `VOETBAL_REGION_LOCK` to a comma-separated list of two-letter country codes to only allow access from those countries. The lock is off by default. When enabled, the app downloads a geo IP database into `/data` on startup (and refreshes it weekly), so mount a volume there to persist it:

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

Requests from private and loopback addresses are always allowed. IP geolocation by [DB-IP](https://db-ip.com) (CC BY 4.0).

## Network lock (optional)

Set `VOETBAL_NETWORK_LOCK` to a comma-separated list of IP addresses, CIDRs, or ASNs to only allow access from those networks. For ASN entries the announced prefixes are fetched on startup from the [RIPEstat announced-prefixes API](https://stat.ripe.net/docs/data-api/api-endpoints/announced-prefixes):

```
docker run --rm -p 8000:8000 \
  -e VOETBAL_NETWORK_LOCK=192.168.0.0/16,1.1.1.1,ASN577 \
  ghcr.io/st3fan/voetbal:main
```

The network lock can be combined with the region lock: the network lock is checked first, and a request is allowed when either lock passes. Requests from private and loopback addresses are always allowed.

## Update

```
docker pull ghcr.io/st3fan/voetbal:main
docker stop voetbal
docker rm voetbal
docker run -d --name voetbal --restart unless-stopped -p 8000:8000 ghcr.io/st3fan/voetbal:main
```
