# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/radicale-extend ./cmd/radicale-extend

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS fonts
RUN apt-get update && apt-get install -y --no-install-recommends fonts-noto-cjk

FROM debian:bookworm-slim AS app
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 app && useradd --uid 10001 --gid 10001 --no-create-home app \
    && install -d -o 10001 -g 10001 /data/state /data/queue
COPY --from=build /out/radicale-extend /usr/local/bin/radicale-extend
COPY --from=fonts /usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc /usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc
COPY --from=fonts /usr/share/doc/fonts-noto-cjk/copyright /usr/share/doc/fonts-noto-cjk/copyright
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s CMD ["radicale-extend","healthcheck"]
ENTRYPOINT ["radicale-extend"]
CMD ["serve","--config","/etc/radicale-extend/config.yaml"]

FROM python:3.13-slim-bookworm AS radicale
RUN groupadd --gid 10001 app && useradd --uid 10001 --gid 10001 --no-create-home app \
    && install -d -o 10001 -g 10001 /data/radicale /data/queue
COPY hook /opt/hook
RUN pip install --no-cache-dir /opt/hook bcrypt==4.3.0
COPY deploy/radicale.conf /etc/radicale/config
COPY deploy/radicale_entrypoint.py deploy/radicale_health.py /opt/
ENV RADICALE_EXTEND_QUEUE=/data/queue/queue.db
USER 10001:10001
EXPOSE 5232
HEALTHCHECK --interval=10s --timeout=5s --start-period=20s CMD ["python","/opt/radicale_health.py"]
ENTRYPOINT ["python","/opt/radicale_entrypoint.py"]
