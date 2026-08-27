# Build a static m314dl binary and package it with ffmpeg (needed for muxing).
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/m314dl .

FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates
COPY --from=build /out/m314dl /usr/local/bin/m314dl
# Downloads land in /data — mount a host directory there.
WORKDIR /data
ENTRYPOINT ["m314dl"]
