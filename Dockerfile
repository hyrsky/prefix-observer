FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /prefix-observer ./cmd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /prefix-observer /prefix-observer
ENTRYPOINT ["/prefix-observer"]
