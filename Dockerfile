# syntax=docker/dockerfile:1

FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X hy2tun/internal/version.Version=${VERSION} -X hy2tun/internal/version.Commit=${COMMIT} -X hy2tun/internal/version.Date=${DATE}" \
      -o /out/hy2tund ./cmd/hy2tund && \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X hy2tun/internal/version.Version=${VERSION} -X hy2tun/internal/version.Commit=${COMMIT} -X hy2tun/internal/version.Date=${DATE}" \
      -o /out/hy2tun ./cmd/hy2tun

FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/hy2tund /usr/local/bin/hy2tund
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/hy2tund"]
CMD ["-c", "/etc/hy2tun/server.yaml"]

FROM gcr.io/distroless/static-debian12:nonroot AS client
COPY --from=build /out/hy2tun /usr/local/bin/hy2tun
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/hy2tun"]
CMD ["-c", "/etc/hy2tun/client.yaml"]
