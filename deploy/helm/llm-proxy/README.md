# llm-proxy Helm Chart

Official Helm chart for deploying [llm-proxy](https://github.com/ai-process/llm-proxy) on Kubernetes.

## Prerequisites

- Kubernetes 1.26+
- Helm v3.8.0+
- A running PostgreSQL database (e.g. AWS RDS, Cloud SQL, or in-cluster)
- (Optional) A Redis instance for rate limiting

## Installing the Chart

### 1. Create Secrets

Create a Kubernetes Secret containing database credentials and the encryption keyring:

```bash
kubectl create secret generic llm-proxy-secrets \
  --from-literal=DATABASE_URL="postgres://user:pass@postgres:5432/llmproxy?sslmode=require" \
  --from-literal=LLMPROXY_ENCRYPTION_KEYS="1:$(head -c 32 /dev/urandom | base64)" \
  --from-literal=BOOTSTRAP_ADMIN_KEY="$(head -c 24 /dev/urandom | base64 | tr -d '/+=')"
```

### 2. Install

```bash
helm install llm-proxy ./deploy/helm/llm-proxy \
  --set secrets.existingSecret=llm-proxy-secrets
```

## Key Configuration Parameters

| Parameter | Description | Default |
| --- | --- | --- |
| `replicaCount` | Number of pods to run | `2` |
| `image.repository` | Container image repository | `coyl/llm-proxy` |
| `image.tag` | Container image tag | `""` (uses `appVersion`) |
| `secrets.existingSecret` | Name of existing Secret with `DATABASE_URL`, `LLMPROXY_ENCRYPTION_KEYS`, `BOOTSTRAP_ADMIN_KEY` | `""` |
| `config.redisAddr` | Optional Redis address for rate limiting | `""` |
| `config.logLevel` | Application log level (`debug`, `info`, `warn`, `error`) | `info` |
| `ingress.enabled` | Enable HTTP/REST Ingress | `false` |
| `ingress.grpc.enabled` | Enable dedicated gRPC Ingress | `false` |
| `autoscaling.enabled` | Enable HorizontalPodAutoscaler | `false` |
| `metrics.serviceMonitor.enabled` | Enable Prometheus Operator ServiceMonitor | `false` |
| `securityContext` | Container security context (non-root by default) | `runAsNonRoot: true`, `runAsUser: 10001` |
