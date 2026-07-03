FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /voetbal . \
    && mkdir -m 0755 /data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /voetbal /voetbal
COPY --from=build --chown=65534:65534 /data /data
VOLUME /data
EXPOSE 8000
USER 65534:65534
ENTRYPOINT ["/voetbal"]
