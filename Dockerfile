# syntax=docker/dockerfile:1

# --- Build stage ---
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder

WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w" -o /out/downlink ./cmd/logserver

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/downlink /usr/local/bin/downlink

VOLUME ["/data"]

EXPOSE 8080
EXPOSE 5514/udp

ENTRYPOINT ["/usr/local/bin/downlink"]
CMD ["-data=/data", "-addr=:8080"]