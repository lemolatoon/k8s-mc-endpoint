FROM --platform=$BUILDPLATFORM docker.io/golang:1.24-bookworm AS builder

ARG TARGETARCH

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} GOOS=linux go build -ldflags="-s -w" -o main

FROM docker.io/debian:bookworm-slim

WORKDIR /root/
COPY --from=builder /app/main .

ENTRYPOINT [ "./main" ]