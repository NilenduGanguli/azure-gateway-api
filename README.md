# azure-gateway-api

A wire-identical gateway for two on-prem Azure AI containers, adding the durable async lifecycle
they lack outside the Azure ecosystem.

| Upstream | Surface served |
|---|---|
| Document Intelligence `prebuilt-layout` | `/documentintelligence/**`, `api-version=2024-11-30` |
| Computer Vision Read v3.2 | `/vision/v3.2/read/**`, `model-version=2022-04-30` |

Unmodified Azure SDK clients cannot tell the gateway from the container. Sync calls stream straight
through. Async calls get a real `202 + Operation-Location`, while the gateway calls the container
**synchronously**, persists the result to a PVC, and serves every poll from its own store — so
operation ids survive pod rescheduling, autoscaling and restarts.

- Design: [`docs/superpowers/specs/2026-09-08-azure-gateway-api-design.md`](docs/superpowers/specs/2026-09-08-azure-gateway-api-design.md)
- API surface reference: [`docs/api-surface.md`](docs/api-surface.md)

## Deployment constraints

Two are real and non-obvious:

1. **Raise the OpenShift Route timeout.** Routes default to 30s; a synchronous call on a large PDF
   holds the connection for minutes. Set `haproxy.router.openshift.io/timeout`.
2. **No `vision` substring in the Read upstream's Service name.** The Read container corrupts its own
   `Operation-Location` when the inbound authority contains `vision`, stripping the port and the
   `/vision` path segment. The gateway mints its own header and is immune, but the constraint is real.
