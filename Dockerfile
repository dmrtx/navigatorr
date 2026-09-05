FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o navigatorr .

FROM alpine:latest
# ffmpeg provides ffprobe, used by inspect_media for real-file inspection
# (container, codec, audio/subtitle languages). Inspection degrades to
# extension/size heuristics when ffprobe is absent, so this stays optional
# but strongly recommended.
RUN apk add --no-cache ca-certificates ffmpeg
COPY --from=builder /app/navigatorr /usr/local/bin/navigatorr
ENTRYPOINT ["navigatorr"]
