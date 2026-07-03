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

## Update

```
docker pull ghcr.io/st3fan/voetbal:main
docker stop voetbal
docker rm voetbal
docker run -d --name voetbal --restart unless-stopped -p 8000:8000 ghcr.io/st3fan/voetbal:main
```
