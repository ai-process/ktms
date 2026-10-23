# Keystone. The Model Switchboard. (KTMS): Helm chart

Official Helm chart for deploying [KTMS (Keystone: The Model Switchboard)](https://github.com/ai-process/ktms) on Kubernetes.

## Prerequisites

- Kubernetes 1.26+
- Helm v3.8.0+
- A running PostgreSQL database (e.g. AWS RDS, Cloud SQL, or in-cluster)
- (Optional) A Redis instance for rate limiting

## Installing the Chart

### 1. Create Secrets

Create a Kubernetes Secret containing database credentials and the encryption keyring:

```bash
kubectl create secret generic ktms-secrets \
  --from-literal=DATABASE_URL="postgres://user:pass@postgres:5432/ktms?sslmode=require" \
  --from-literal=KTMS_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)" \
  --from-literal=BOOTSTRAP_ADMIN_KEY="$(head -c 24 /dev/urandom | base64 | tr -d '/+=')"
```

### 2. Install

```bash
helm install ktms ./deploy/helm/ktms \
  --set secrets.existingSecret=ktms-secrets
```

Or install with inline secrets:

```bash
helm install ktms ./deploy/helm/ktms \
  --set secrets.databaseUrl="postgres://user:pass@postgres:5432/ktms?sslmode=require" \
  --set secrets.encryptionKeys="1:$(head -c 32 /dev/urandom | base64)"
```

## Key Configuration Parameters

| Parameter | Description | Default |
| --- | --- | --- |
| `replicaCount` | Number of pods to run | `2` |
| `image.repository` | Container image repository | `coyl/ktms` |
| `image.tag` | Container image tag | `""` (uses `appVersion`) |
| `secrets.existingSecret` | Name of existing Secret with `DATABASE_URL`, `KTMS_ENCRYPTION_KEYS`, `BOOTSTRAP_ADMIN_KEY`, `REDIS_PASSWORD`, `USAGE_COLLECTOR_TOKEN` | `""` |
| `config.routingReloadSeconds` | Replicas config poll interval in seconds | `60` |
| `config.redisAddr` | Optional Redis address for rate limiting | `""` |
| `config.vendorCooldownMax` | Longest pause honoured when a vendor answers `429`/`503` with `Retry-After` (`"0"` disables pauses) | `5m` |
| `config.spendLimitsFailMode` | Spend limit fail mode (`closed` fails safe, `open` allows unmetered; chart defaults to `closed` for production safety) | `closed` |
| `config.usageCollectorHost` / `config.usageCollectorTLS` | Optional gRPC usage sink (`host:port`) and whether to reach it over TLS | `""` / `false` |
| `secrets.usageCollectorToken` | Bearer token for the usage sink (light-tokenmeter `write` scope) | `""` |
| `config.logLevel` | Application log level (`debug`, `info`, `warn`, `error`) | `info` |
| `probes.startup.enabled` | Startup probe to accommodate database schema migrations | `true` |
| `service.httpPort` | Kubernetes Service HTTP port (mapped to container :8080) | `8080` |
| `service.grpcPort` | Kubernetes Service gRPC port (mapped to container :9090) | `9090` |
| `service.metricsPort` | Kubernetes Service metrics port (mapped to container :9091) | `9091` |
| `ingress.enabled` | Enable HTTP/REST Ingress | `false` |
| `ingress.grpc.enabled` | Enable dedicated gRPC Ingress | `false` |
| `autoscaling.enabled` | Enable HorizontalPodAutoscaler | `false` |
| `podDisruptionBudget.enabled` | Enable PodDisruptionBudget | `false` |
| `podDisruptionBudget.minAvailable` | Minimum available pods during disruptions (default `1` when enabled; overridden by `maxUnavailable`) | `1` |
| `podDisruptionBudget.maxUnavailable` | Maximum unavailable pods during disruptions (e.g. `1` or `"25%"`) | `""` |
| `metrics.enabled` | Expose metrics port on Service and container (listener runs on :9091) | `true` |
| `metrics.serviceMonitor.enabled` | Enable Prometheus Operator ServiceMonitor | `false` |
| `podSecurityContext` | Pod security context (non-root, seccomp RuntimeDefault) | `runAsNonRoot: true`, `runAsUser: 10001` |
| `securityContext` | Container security context (drop ALL, read-only rootfs) | `readOnlyRootFilesystem: true` |
| `terminationGracePeriodSeconds` | Pod termination grace period in seconds | `60` |
| `lifecycle.preStop` | PreStop hook to drain traffic before shutdown | `sleep 5` |

> [!NOTE]
> The internal container ports (`:8080` HTTP, `:9090` gRPC, `:9091` metrics) are fixed and mapped via named Service targetPorts. Changing `service.httpPort`, `service.grpcPort`, or `service.metricsPort` alters the external cluster Service ports without breaking internal container probes. Do not override `HTTP_ADDR`, `GRPC_ADDR`, or `METRICS_ADDR` via `extraEnv` as doing so would desynchronize internal listener ports from container port bindings and health probes.
