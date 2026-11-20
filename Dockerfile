FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o navigatorr .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o navigatorr-nas-helper ./cmd/navigatorr-nas-helper
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o navigatorr-gateway ./cmd/navigatorr-gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o navigatorr-nodes ./cmd/navigatorr-nodes

# The read-only helper that runs next to the library on the NAS (docs/nas-helper.md).
# Built with --target nas-helper; the last stage below stays the default image.
FROM scratch AS nas-helper
COPY --from=builder /app/navigatorr-nas-helper /navigatorr-nas-helper
ENTRYPOINT ["/navigatorr-nas-helper"]

FROM alpine:latest
# ffmpeg provides ffprobe, used by inspect_media for real-file inspection
# (container, codec, audio/subtitle languages). Inspection degrades to
# extension/size heuristics when ffprobe is absent, so this stays optional
# but strongly recommended.
RUN apk add --no-cache ca-certificates ffmpeg openssh-client
COPY --from=builder /app/navigatorr /usr/local/bin/navigatorr
# The MCP gateway ships in the same image; run it with --entrypoint navigatorr-gateway.
COPY --from=builder /app/navigatorr-gateway /usr/local/bin/navigatorr-gateway
# The Nodes builds registry ships in the same image; run it with --entrypoint navigatorr-nodes.
COPY --from=builder /app/navigatorr-nodes /usr/local/bin/navigatorr-nodes
ENTRYPOINT ["navigatorr"]
