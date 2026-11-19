# Build stage: Go version matches go.mod.
FROM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/admin ./cmd/admin

# Runtime stage: docker CLI with the compose plugin (used by "recreate").
FROM docker:29-cli

WORKDIR /app
COPY --from=builder /out/admin /app/admin

EXPOSE 8080

CMD ["/app/admin"]
