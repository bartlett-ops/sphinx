# Sphinx

Sphinx is a self-registration authentication service for Kubernetes clusters running [Traefik](https://traefik.io). It exposes a small HTTP API that records a user's email and client IP, then keeps a Traefik `Middleware` IP allowlist in sync so that only registered users can reach protected services.

When a user hits the registration endpoint (typically via a Traefik `ForwardAuth` rule), Sphinx:

1. Reads the `X-User-Email` header (set by your identity provider / auth proxy).
2. Records the client IP alongside the email.
3. Patches the configured Traefik `Middleware` `ipAllowList` with the new IP.
4. Persists the user list to a Kubernetes `ConfigMap` so state survives restarts.

Multiple Sphinx replicas co-exist safely — each instance owns its own key in the `ConfigMap` (keyed by pod hostname) using Kubernetes Server-Side Apply, so replicas never overwrite each other.

## API

| Method | Path     | Description                                      |
|--------|----------|--------------------------------------------------|
| `GET`  | `/users` | Returns the current in-memory user map as JSON.  |
| `POST` | `/users` | Registers the caller's IP against their email.   |

`POST /users` reads the `X-User-Email` request header for the email address and derives the client IP from `X-Forwarded-For` (first entry) or the direct connection address.

## Configuration

All flags can be set via environment variables by uppercasing the flag name, replacing `-` with `_`, and prepending `SPHINX_` (e.g. `--middleware-name` → `SPHINX_MIDDLEWARE_NAME`).

| Flag                    | Env var                       | Default            | Required | Description                                                                 |
|-------------------------|-------------------------------|--------------------|----------|-----------------------------------------------------------------------------|
| `--middleware-name`     | `SPHINX_MIDDLEWARE_NAME`      | —                  | Yes      | Name of the Traefik `Middleware` resource to manage.                        |
| `--middleware-namespace`| `SPHINX_MIDDLEWARE_NAMESPACE` | `kube-system`      | No       | Kubernetes namespace containing the middleware and the user `ConfigMap`.    |
| `--configmap-name`      | `SPHINX_CONFIGMAP_NAME`       | `sphinx-users`     | No       | Name of the `ConfigMap` used to persist user registrations.                 |
| `--port`                | `SPHINX_PORT`                 | `8080`             | No       | Port the HTTP server listens on.                                            |
| `--trusted-proxies`     | `SPHINX_TRUSTED_PROXIES`      | —                  | No       | Comma-separated list of trusted proxy CIDRs (passed to Gin).               |
| `--kubeconfig`          | `SPHINX_KUBECONFIG`           | —                  | No       | Path to a kubeconfig file. Auto-detected: in-cluster config when running as a pod, otherwise `~/.kube/config`. |

## Running locally

```sh
make dev
```

This runs:

```sh
go run . --middleware-name sphinx-allowlist --middleware-namespace kube-system --trusted-proxies 0.0.0.0/0
```

Sphinx will use `~/.kube/config` automatically when running outside a cluster.

## Deployment

Build the image:

```sh
docker build -t sphinx .
```

The image is based on `distroless/static` and runs as a non-root user. When deployed as a pod, Sphinx detects the in-cluster service account credentials automatically — no `--kubeconfig` flag required.

The pod's service account needs the following RBAC permissions in the middleware namespace:

- `get`, `create`, `update` on `middlewares.traefik.io`
- `get`, `patch` on `configmaps`
