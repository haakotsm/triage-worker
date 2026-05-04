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

# Test
go test ./...

# Test with race detector
go test -race -count=1 ./...

# Run locally (requires Temporal and services)
export TEMPORAL_ADDRESS=localhost:7233
export DATABASE_URL=postgres://user:pass@localhost:5432/triage?sslmode=disable
./triage-worker

# Docker
docker build -t triage-worker .
docker run -p 8080:8080 triage-worker
```

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
- Body size limit (1MB) on webhook endpoint
- Non-retryable error classification prevents infinite retry loops

## Operator Guide

### Prerequisites

Before deploying, ensure:
1. **Temporal** running with `k8s-triage` task queue (auto-created on worker start)
2. **kagent** with `error-triage-agent` Agent CRD deployed
3. **agentgateway** with triage HTTPRoute and policy
4. **Keycloak** with `triage-worker` client (client-credentials grant)
5. **PostgreSQL** accessible with `triage` schema permissions

### Pre-register Search Attributes

```bash
temporal operator search-attribute create \
  --name TriageNamespace --type Text \
  --name TriageWorkload --type Text \
  --name TriageClassification --type Text \
  --name TriageSeverity --type Text
```

### Deployment

Deployed via ArgoCD from `ops/argocd/platform/kagent/triage/`. Manifests:
- `deployment.yaml` — Worker Deployment + Service
- `agent.yaml` — kagent Agent CRD
- `rbac.yaml` — ServiceAccount + ClusterRole (read-only)
- `network-policy.yaml` — Egress restrictions

### Monitoring

- **Temporal UI**: `http://temporal.localhost` → search by TaskQueue `k8s-triage`
- **Health probe**: `GET :8080/readyz` — checks Temporal connectivity
- **Logs**: structured JSON with `component`, `workflow_id`, `namespace`

### Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| /readyz returns 503 | Temporal unreachable | Check `temporal-frontend.temporal.svc:7233` |
| Webhook returns 500 | SignalWithStart failed | Check worker logs, Temporal health |
| Agent returns ParseError | LLM output not valid JSON | Check agent system prompt, model size |
| Auth rejected (401) | Token expired or wrong client | Check Keycloak `triage-worker` client secret |
| Rate limited (429) | Too many A2A calls | Check agentgateway policy rate limit |

### Progressive Automation Levels

| Level | Behavior | Status |
|-------|----------|--------|
| L0 Inform | Triage report only | ✅ Active |
| L1 Suggest | Report + diagnostic kubectl commands | ✅ Active |
| L2 Assist | Auto-execute read diagnostics | 🔮 Future |
| L3 Remediate | Safe fixes with human approval | 🔮 Future |

## License

Private — Bodils Bibliotek Platform
