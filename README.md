# triage-worker

Kubernetes error triage worker — Temporal workflow orchestration + kagent A2A integration.

## Overview

Receives Alertmanager webhook notifications, correlates related alerts using Temporal signal aggregation, enriches context from Prometheus/Loki/K8s API, and invokes a kagent AI agent for root cause diagnosis.

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
| `DATABASE_URL` | — | PostgreSQL connection string |
| `LISTEN_ADDR` | `:8080` | HTTP server listen address |
| `LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |

## Development

```bash
# Build
go build -o triage-worker ./cmd/worker

# Run locally (requires Temporal and services)
export TEMPORAL_ADDRESS=localhost:7233
export DATABASE_URL=postgres://user:pass@localhost:5432/triage?sslmode=disable
./triage-worker

# Docker
docker build -t triage-worker .
docker run -p 8080:8080 triage-worker
```

## API

### POST /webhook

Alertmanager webhook endpoint. Receives alert groups and starts/signals Temporal workflows.

### GET /healthz

Liveness probe — always returns 200.

### GET /readyz

Readiness probe — returns 200 only when Temporal is reachable.

## Security

- Authenticates to agentgateway via OAuth2 client-credentials (Keycloak)
- Read-only RBAC — no write access to the cluster
- NetworkPolicy blocks direct kagent access (must go through agentgateway)
- Input sanitization on all telemetry data before passing to agent

## License

Private — Bodils Bibliotek Platform
