# syntax=docker/dockerfile:1

# ---- streagit: on-demand page renderer (Rust, static musl), pinned release
FROM rust:alpine AS streagit
ARG STREAGIT_REF=v0.1.3
RUN apk add --no-cache musl-dev
ADD https://github.com/MarvAmBass/streagit.git#${STREAGIT_REF} /src
WORKDIR /src
RUN cargo build --release --locked && strip target/release/streagit
# its license files (THIRD-PARTY-LICENSES* only exists in newer releases)
RUN mkdir -p /licenses && for f in LICENSE* THIRD-PARTY-LICENSES*; do if [ -f "$f" ]; then cp "$f" /licenses/; fi; done \
 && test -f /licenses/LICENSE

# ---- gitbackup -----------------------------------------------------------
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/gitbackup ./cmd/gitbackup

# ---- runtime ------------------------------------------------------------
FROM alpine:3
RUN apk add --no-cache git openssh-client ca-certificates tzdata \
 && adduser -D -u 1000 -h /data gitbackup \
 && git config --system --add safe.directory '*'
# safe.directory '*': the /data volume may be owned by another uid than the
# container user; this only affects git inside this container.
COPY --from=build /out/gitbackup /usr/local/bin/gitbackup
COPY --from=streagit /src/target/release/streagit /usr/local/bin/streagit
COPY --from=streagit /src/assets/ /usr/local/share/gitbackup/assets/
COPY LICENSE LICENSE-stagit THIRD_PARTY_NOTICES /usr/share/licenses/gitbackup/
COPY --from=streagit /licenses/ /usr/share/licenses/streagit/
ENV GITBACKUP_HOME=/data \
    GITBACKUP_LISTEN=:8080
VOLUME /data
EXPOSE 8080
USER gitbackup
HEALTHCHECK --interval=1m --timeout=5s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/gitbackup"]
