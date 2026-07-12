# Sphinx

Sphinx is a self-registration authentication service for Kubernetes clusters running [Traefik](https://traefik.io). It exposes a small HTTP API that records a user's email and client IP, then keeps a Traefik `Middleware` IP allowlist in sync so that only registered users can reach protected services.

When a user hits the registration endpoint (typically via a Traefik `ForwardAuth` rule), Sphinx:

1. Reads the `X-Forwarded-User` header (set by your identity provider / auth proxy).
2. Converts the client IP to a single-host CIDR (`/32` for IPv4, `/128` for IPv6).
3. Records the CIDR against the email in a Kubernetes `ConfigMap`, replacing any CIDR previously held for that user.
4. Rewrites the Traefik `Middleware` `ipAllowList` to exactly the set of registered CIDRs.
5. Re-projects the allowlist on a timer (`--reconcile-interval`) to repair drift.

Multiple Sphinx replicas co-exist safely. All replicas share a single `ConfigMap` document keyed by user email, mutated by compare-and-swap on `resourceVersion` and retried on conflict. Each user has exactly one CIDR: re-authenticating from a new address replaces the old one, and the Traefik allowlist is rewritten to exactly the set of registered CIDRs rather than accumulated. Records persist indefinitely; there is no expiry.

## API

| Method | Path      | Description                                                                                                                 |
|--------|-----------|-----------------------------------------------------------------------------------------------------------------------------|
| `GET`  | `/auth`   | Registers the caller's CIDR against their email. This is the Traefik `ForwardAuth` entry point.                              |
| `POST` | `/users`  | Identical to `GET /auth`, retained for backwards compatibility.                                                              |
| `GET`  | `/users`  | Returns the current user records as JSON, read from the `ConfigMap`.                                                        |
| `GET`  | `/health` | Liveness probe. Returns `200` whenever the process is serving.                                                               |
| `GET`  | `/ready`  | Readiness probe. Returns `200` only when a reconcile has succeeded recently and both the `Middleware` and `ConfigMap` are reachable. |

`GET /auth` and `POST /users` read the `X-Forwarded-User` request header for the email address, and derive the client IP from `X-Forwarded-For` (first entry) or the direct connection address. They return `201 Created` when the registration was written, and `200 OK` when the CIDR was unchanged and the request was served from the pod's write-skip cache.

## Configuration

All flags can be set via environment variables by uppercasing the flag name, replacing `-` with `_`, and prepending `SPHINX_` (e.g. `--middleware-name` → `SPHINX_MIDDLEWARE_NAME`).

| Flag                    | Env var                       | Default            | Required | Description                                                                 |
|-------------------------|-------------------------------|--------------------|----------|-----------------------------------------------------------------------------|
| `--middleware-name`     | `SPHINX_MIDDLEWARE_NAME`      | —                  | Yes      | Name of the Traefik `Middleware` resource to manage.                        |
| `--middleware-namespace`| `SPHINX_MIDDLEWARE_NAMESPACE` | `kube-system`      | No       | Kubernetes namespace containing the middleware and the user `ConfigMap`.    |
| `--configmap-name`      | `SPHINX_CONFIGMAP_NAME`       | `sphinx-users`     | No       | Name of the `ConfigMap` used to persist user registrations.                 |
| `--reconcile-interval`  | `SPHINX_RECONCILE_INTERVAL`   | `60s`              | No       | How often the allowlist is re-projected from the store to repair drift.     |
| `--port`                | `SPHINX_PORT`                 | `8080`             | No       | Port the HTTP server listens on.                                            |
| `--trusted-proxies`     | `SPHINX_TRUSTED_PROXIES`      | —                  | Yes, behind a proxy | CIDRs of the reverse proxies in front of Sphinx. The client IP is taken from the first `X-Forwarded-For` entry that is not one of these. **Do not use `0.0.0.0/0`** — it trusts every address, which lets any caller choose the CIDR that gets allowlisted. |
| `--kubeconfig`          | `SPHINX_KUBECONFIG`           | —                  | No       | Path to a kubeconfig file. Auto-detected: in-cluster config when running as a pod, otherwise `~/.kube/config`. |

## Running locally

```sh
make dev
```

This runs:

```sh
go run . --middleware-name sphinx-dev --configmap-name sphinx-dev-users --middleware-namespace kube-system --trusted-proxies 127.0.0.1/32 --reconcile-interval 30s
```

Sphinx will use `~/.kube/config` automatically when running outside a cluster. The local target uses scratch resources (`sphinx-dev`, `sphinx-dev-users`) so a local run cannot rewrite a live allowlist.

## Security

Sphinx writes the caller's address into an IP allowlist, so the address it derives must not be attacker-controlled.

The client IP comes from gin's `ClientIP()`, which walks `X-Forwarded-For` from right to left and returns the first entry that is not listed in `--trusted-proxies`. Set `--trusted-proxies` to the CIDR of your reverse proxy — for a Traefik pod, that is the cluster's pod CIDR.

A catch-all value such as `0.0.0.0/0` marks every address as a trusted proxy. Gin then falls through to the leftmost `X-Forwarded-For` entry, which any client can set, and an attacker can have an arbitrary CIDR allowlisted. Sphinx logs a warning at startup if it detects this, but it cannot refuse to run — some deployments legitimately terminate TLS elsewhere.

## Deployment

Build the image:

```sh
docker build -t sphinx .
```

The image is based on `distroless/static` and runs as a non-root user. When deployed as a pod, Sphinx detects the in-cluster service account credentials automatically — no `--kubeconfig` flag required.

The pod's service account needs the following RBAC permissions in the middleware namespace:

- `get`, `create`, `update` on `middlewares.traefik.io`
- `get`, `create`, `update` on `configmaps`
