# Build the dbrest binary in a Go 1.26 builder, then copy it into a minimal
# Alpine image. The binary is statically linked (CGO_ENABLED=0) so it runs on
# any Linux base image without a matching libc.
FROM docker.io/library/golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/dbrest ./cmd/dbrest

FROM docker.io/library/alpine:3.21
COPY --from=builder /bin/dbrest /usr/local/bin/dbrest
EXPOSE 3001
ENTRYPOINT ["/usr/local/bin/dbrest"]
