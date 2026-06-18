# glibc (not -alpine) so Tailwind's native oxide engine matches the CI drift
# check (ubuntu/glibc) and local builds — guarantees byte-identical output.css
# across all three. This stage is discarded; only /output.css is copied out.
FROM node:22 AS css-builder

WORKDIR /src
COPY .css-build/package.json .css-build/package-lock.json ./.css-build/
RUN cd .css-build && npm ci --no-audit --no-fund
COPY .css-build/app.css ./.css-build/
# app.css @source scans templates, the Go handlers, and init.js (class names are
# generated in Go/JS, not just templates), so all three must be present here.
COPY internal/web/templates/ ./internal/web/templates/
COPY internal/web/*.go ./internal/web/
COPY internal/web/static/init.js ./internal/web/static/init.js
RUN cd .css-build && npx @tailwindcss/cli -i app.css -o /output.css --minify

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
