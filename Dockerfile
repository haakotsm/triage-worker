FROM node:22-alpine AS css-builder

WORKDIR /src
COPY .css-build/ ./.css-build/
COPY internal/web/templates/ ./internal/web/templates/
RUN cd .css-build \
  && npm install --no-audit --no-fund \
  && npx @tailwindcss/cli -i app.css -o /output.css --minify

# ---
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=css-builder /output.css ./internal/web/static/output.css
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /triage-worker ./cmd/worker

# ---
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /triage-worker /triage-worker

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/triage-worker"]
