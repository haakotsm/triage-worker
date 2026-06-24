# triage-worker

Kubernetes error triage worker — Temporal workflow orchestration + kagent A2A integration.

## Overview

Receives Alertmanager webhook notifications, correlates related alerts using Temporal signal aggregation, enriches context from Prometheus/Loki/K8s API, and invokes a kagent AI agent (via agentgateway) for root cause diagnosis. Persists structured reports to PostgreSQL and serves a server-rendered web dashboard (htmx + Alpine + SSE) for operator triage.

## Architecture

```
Alertmanager → webhook/handler.go → Temporal SignalWithStart
                                          │
                                    TriageWorkflow
                                          │
                              ┌───────────┼───────────┐
                              ▼           ▼           ▼
                        Prometheus    K8s API       Loki
                        (enrich)     (enrich)    (enrich)
                              └───────────┬───────────┘
                                          ▼
                                  kagent Agent (A2A)
                                  via agentgateway
                                          │
                                          ▼
                                  PostgreSQL (report)
                                          │
                                          │ PG LISTEN/NOTIFY
                                          ▼
                              Web Dashboard (htmx + SSE)
                              /dashboard · /events · /api/*
```

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `TEMPORAL_ADDRESS` | `temporal-frontend.temporal.svc.cluster.local:7233` | Temporal frontend address |
| `TEMPORAL_NAMESPACE` | `default` | Temporal namespace |
| `TEMPORAL_TASK_QUEUE` | `k8s-triage` | Task queue name |
| `KAGENT_A2A_URL` | `http://agentgateway.agentgateway.svc.cluster.local:3001` | Agentgateway base URL |
| `KAGENT_AGENT_NAMESPACE` | `kagent` | Agent CRD namespace |
| `KAGENT_AGENT_NAME` | `error-triage-agent` | Agent CRD name |
| `PROMETHEUS_URL` | `http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090` | Prometheus API |
| `LOKI_URL` | `http://loki.monitoring.svc.cluster.local:3100` | Loki API |
| `KEYCLOAK_TOKEN_URL` | `http://keycloak.keycloak.svc.cluster.local/realms/bibliotek/protocol/openid-connect/token` | OAuth2 token endpoint |
| `KEYCLOAK_CLIENT_ID` | `triage-worker` | OAuth2 client ID |
| `KEYCLOAK_CLIENT_SECRET` | — | OAuth2 client secret |
| `DATABASE_URL` | — | PostgreSQL connection string (enables report persistence, API and web dashboard) |
| `WEBHOOK_SECRET` | — | Bearer token required on the Alertmanager webhook (empty = unauthenticated) |
| `DEV_MODE` | `false` | When `true`, web dashboard injects a synthetic dev user and bypasses upstream auth headers |
| `LISTEN_ADDR` | `:8080` | Dashboard/webhook HTTP server listen address (public via ingress) |
| `METRICS_ADDR` | `:9090` | Prometheus `/metrics` listen address (separate from the public dashboard port; not exposed via ingress) |
| `LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |

## Development

```bash
# Build
go build -o triage-worker ./cmd/worker

# Test
go test ./...

# Test with race detector
go test -race -count=1 ./...

# Run locally (requires Temporal and services)
export TEMPORAL_ADDRESS=localhost:7233
export DATABASE_URL=postgres://user:pass@localhost:5432/triage?sslmode=disable
export DEV_MODE=true   # bypass auth headers when accessing /dashboard locally
./triage-worker

# Docker
docker build -t triage-worker .
docker run -p 8080:8080 triage-worker
```

### Dashboard CSS

Tailwind + daisyUI styles are embedded via `go:embed` from `internal/web/static/output.css`. The Docker build regenerates this file from `.css-build/` on every image build (multi-stage with `node:22-alpine`), so the deployed image always matches the templates.

The runtime container itself ships no Node.js. The committed `output.css` is used by `go run` for local dev only — it may drift from templates between Docker builds. To refresh it locally:

```bash
cd .css-build
npm ci
npx @tailwindcss/cli -i app.css -o ../internal/web/static/output.css --minify
```

`.css-build/node_modules/` is gitignored; `package.json`, `package-lock.json`, and `app.css` are tracked so both Docker and local rebuilds use byte-identical transitive deps.

## Test Scenarios

| ID | File | Scenario | Validates |
|----|------|----------|-----------|
| S1 | `testdata/scenarios/s1_crashloop.json` | CrashLoopBackOff | Single alert classification |
| S2 | `testdata/scenarios/s2_oom.json` | OOMKilled | Memory metric correlation |
| S3 | `testdata/scenarios/s3_network_policy.json` | NetworkPolicy block | Policy identification |
| S4 | `testdata/scenarios/s4_cascade.json` | Cascading DB failure | **Multi-alert correlation** |
| S5 | `testdata/scenarios/s5_imagepull.json` | ImagePullBackOff | Event parsing |
| S6 | `testdata/scenarios/s6_resource_exhaustion.json` | Node resource pressure | Node-level diagnosis |

To test manually with curl:
```bash
curl -X POST http://localhost:8080/webhook \
  -H "Content-Type: application/json" \
  -d @testdata/scenarios/s1_crashloop.json
```

## API

### Webhook & health

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/webhook` | Alertmanager webhook. Requires `Authorization: Bearer $WEBHOOK_SECRET` when set, then calls `SignalWithStart` on `TriageWorkflow`. |
| `GET` | `/healthz` | Liveness probe — always 200. |
| `GET` | `/readyz` | Readiness probe — 200 only when Temporal is reachable. |

### JSON API (enabled when `DATABASE_URL` is set)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/reports` | List recent triage reports (paginated). |
| `GET` | `/api/reports/active` | List unresolved reports. |
| `GET` | `/api/reports/{id}` | Fetch a single report by ID. |

### Web dashboard (enabled when `DATABASE_URL` is set)

Server-rendered htmx + Alpine UI gated by upstream auth proxy headers (`X-Auth-Request-Email`, `X-Auth-Request-User`, `X-Auth-Request-Groups`). Set `DEV_MODE=true` to bypass for local development.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` · `/dashboard` | Triage operator dashboard (list + stats). |
| `GET` | `/incidents/{id}` | Incident detail view. |
| `GET` | `/reports/{id}` | Legacy alias — 301 redirects to `/incidents/{id}`. |
| `GET` | `/events` | SSE stream of dashboard updates (backed by PostgreSQL `LISTEN/NOTIFY`). |
| `GET` | `/partials/{reports,stats,incidents}` | htmx fragments for live refresh. |
| `POST` | `/api/incidents/{id}/{acknowledge,escalate,notes,retriage}` | Operator actions (CSRF-protected). |
| `POST` | `/reports/{id}/resolve` · `/incidents/{id}/resolve` | Resolve a report (CSRF-protected). |
| `GET` | `/static/*` | Embedded CSS/JS assets (Tailwind output, htmx, Alpine, SSE shim). |

## Security

- Authenticates to agentgateway via OAuth2 client-credentials (Keycloak)
- Read-only RBAC — no write access to the cluster
- NetworkPolicy blocks direct kagent access (must go through agentgateway)
- Input sanitization on all telemetry data before passing to agent
- Body size limit (1MB) on webhook endpoint
- Bearer-token-authenticated Alertmanager webhook when `WEBHOOK_SECRET` is set (constant-time comparison)
- Non-retryable error classification prevents infinite retry loops
- Web dashboard requires upstream auth proxy headers; all state-changing requests are CSRF-protected (double-submit token)
- Distroless runtime image (`gcr.io/distroless/static-debian12:nonroot`) — no shell. For in-cluster debugging use `kubectl debug` with an ephemeral container rather than expecting `wget`/`curl` to be present

## Operator Guide

### Prerequisites

Before deploying, ensure:
1. **Temporal** running with `k8s-triage` task queue (auto-created on worker start)
2. **kagent** with `error-triage-agent` Agent CRD deployed
3. **agentgateway** with triage HTTPRoute and policy
4. **Keycloak** with `triage-worker` client (client-credentials grant)
5. **PostgreSQL** accessible with `triage` schema permissions

### Pre-register Search Attributes

Search attributes are automatically registered by the `temporal-register-search-attributes`
Job (deployed via ArgoCD PostSync hook). For manual registration:

```bash
temporal operator search-attribute create \
  --name TriageNamespace --type Keyword \
  --name TriageWorkload --type Keyword \
  --name TriageClassification --type Keyword \
  --name TriageSeverity --type Keyword
```

### Deployment

Deployed via ArgoCD from `ops/argocd/platform/kagent/triage/`. Manifests:
- `deployment.yaml` — Worker Deployment + Service
- `agent.yaml` — kagent Agent CRD
- `rbac.yaml` — ServiceAccount + ClusterRole (read-only)
- `network-policy.yaml` — Egress restrictions

### Monitoring

- **Temporal UI**: `http://temporal.localhost` → search by TaskQueue `k8s-triage`
- **Health probe**: `GET :8080/readyz` — checks Temporal connectivity. The runtime image is distroless and has no shell, so probes must use Kubernetes `httpGet` (not `exec` + `wget`/`curl`). For ad-hoc checks use `kubectl debug pod/<name> --image=busybox --target=triage-worker -- wget -qO- http://localhost:8080/readyz`.
- **Web dashboard**: `GET :8080/dashboard` — live operator view (requires auth proxy in front)
- **Logs**: structured JSON with `component`, `workflow_id`, `namespace`

### Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| /readyz returns 503 | Temporal unreachable | Check `temporal-frontend.temporal.svc:7233` |
| Webhook returns 500 | SignalWithStart failed | Check worker logs, Temporal health |
| Webhook returns 401 | Bearer token mismatch | Verify Alertmanager `http_config.authorization.credentials` matches `WEBHOOK_SECRET` |
| Agent returns ParseError | LLM output not valid JSON | Check agent system prompt, model size |
| Auth rejected (401) | Token expired or wrong client | Check Keycloak `triage-worker` client secret |
| Rate limited (429) | Too many A2A calls | Check agentgateway policy rate limit |
| Dashboard 401 | Missing upstream auth headers | Ensure oauth2-proxy fronts the route, or set `DEV_MODE=true` locally |
| Dashboard `/events` stalls | SSE broker failed to start (no PG `LISTEN`) | Verify `DATABASE_URL` reachable; check startup log for `SSE broker failed to start` |

### Progressive Automation Levels

| Level | Behavior | Status |
|-------|----------|--------|
| L0 Inform | Triage report only | ✅ Active |
| L1 Suggest | Report + diagnostic kubectl commands | ✅ Active |
| L2 Assist | Auto-execute read diagnostics | 🔮 Future |
| L3 Remediate | Safe fixes with human approval | 🔮 Future |

## License

Private — Bodils Bibliotek Platform
