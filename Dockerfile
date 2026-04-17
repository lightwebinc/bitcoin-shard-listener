FROM golang:1.25 AS builder
WORKDIR /src

COPY go.mod go.sum ./
COPY ../bitcoin-shard-proxy /proxy
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-X github.com/lightwebinc/bitcoin-shard-listener/metrics.Version=${VERSION} -buildvcs=false" \
    -o /bitcoin-shard-listener .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bitcoin-shard-listener /bitcoin-shard-listener
ENTRYPOINT ["/bitcoin-shard-listener"]
