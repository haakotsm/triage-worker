FROM golang:1.26-alpine AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /triage-worker ./cmd/worker

# ---
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /triage-worker /triage-worker

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/triage-worker"]
