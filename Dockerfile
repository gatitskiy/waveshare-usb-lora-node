FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git protoc protobuf-dev

RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

WORKDIR /src

COPY go.mod go.sum ./
COPY gen/github.com/meshtastic/go/generated/go.mod ./gen/github.com/meshtastic/go/generated/go.mod
RUN go mod download

COPY . .

RUN if [ ! -f protobufs/meshtastic/mesh.proto ]; then \
        rm -rf protobufs && \
        git clone --depth 1 https://github.com/meshtastic/protobufs.git protobufs; \
    fi

RUN go generate ./...

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/ws-node ./cmd/ws-node \
    && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/ws-ntfy ./cmd/ws-ntfy


FROM alpine:3.20

RUN apk add --no-cache ca-certificates tini

COPY --from=builder /out/ws-node /usr/local/bin/ws-node
COPY --from=builder /out/ws-ntfy /usr/local/bin/ws-ntfy

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["ws-node", "-h"]
