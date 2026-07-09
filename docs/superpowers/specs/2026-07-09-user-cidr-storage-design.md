# User/CIDR Storage Redesign

**Date:** 2026-07-09
**Status:** Approved, pending implementation

## Problem

When a Sphinx pod is replaced and a user then re-authenticates from a new address, the
user's old CIDR remains in the Traefik allowlist. A single user accumulates multiple
allowlisted CIDRs, and CIDRs belonging to replaced pods' users are never removed.

Two compounding flaws in the current design cause this.

**Data is sharded by pod, not by user.** Persistence uses a single ConfigMap in which each
replica owns a key named after its own hostname (`instanceID`), writing its entire
in-memory user map as JSON under that key via Server-Side Apply. A pod hostname is not a
property of the data. When a pod is replaced, its key becomes an orphan that nothing
reclaims, and the replacement pod — reading only its own key — starts empty.

**The middleware is append-only.** `mutate` unions new CIDRs into the existing
`spec.ipAllowList.sourceRange` (`middleware.go:98`). No code path ever removes a CIDR, so
re-authenticating from a new address adds the new CIDR while the old one stays allowlisted
indefinitely.

## Objective

A user has exactly one CIDR. Re-authenticating from a new address removes the old CIDR.
The system must remain correct across multiple pod replicas and leave no stale data behind
when a pod is replaced.

## Guiding principle

The Traefik `Middleware` is a **derived projection** of the user store, never a mutable
accumulator. Every write path is: mutate the store, recompute the complete desired CIDR
set, write that exact set to the middleware.

Records are keyed by **email**, never by pod.

Together these make the bug structurally impossible rather than merely fixed: one CIDR per
user follows from email being a map key, orphans cannot exist because no key is derived
from a pod, and removals propagate because `Apply` writes the whole set authoritatively.

## Approach

Retain the ConfigMap, but store a **single shared JSON document** keyed by email, mutated
via compare-and-swap on `resourceVersion`.

Alternatives considered and rejected:

- **One ConfigMap key per user.** ConfigMap keys admit only alphanumerics, `-`, `_`, and
  `.`; `@` is illegal, so every email would need encoding or hashing. This buys lock-free
  per-key writes we do not need, and costs both readability and the ability to rewrite the
  whole record set atomically — which the migration path depends on.
- **A `SphinxUser` custom resource per user.** The textbook Kubernetes answer, granting
  per-object atomicity for free. Rejected as disproportionate: it requires shipping and
  versioning a CRD, expanding RBAC, and adopting informer machinery to solve a problem one
  ConfigMap solves. Revisit if Sphinx grows to manage many resource types.
- **External store (Redis).** Introduces a stateful dependency requiring persistence
  configuration and credential management. The dependency does not pay for itself for a
  small allowlist that Kubernetes already stores.

## Architecture

The current code entangles global mutable state, Traefik types, ConfigMap persistence, and
HTTP handlers. The redesign splits along the seam that matters: **the unit that stores
users must not know what Traefik is, and the unit that writes Traefik must not know what a
user is.**

### `store.go` — the user store

Owns the ConfigMap. Handles compare-and-swap retries internally. Knows nothing of Traefik
or HTTP. Each method returns the resulting authoritative `*Store`.

```go
type UserStore interface {
    Load(ctx context.Context) (*Store, error)
    Upsert(ctx context.Context, email, cidr string, now time.Time) (*Store, error)
}
```

Records persist indefinitely. There is no expiry and no `Sweep`; the only way a record
changes is an `Upsert` for that email.

### `allowlist.go` — the Traefik writer

Owns the `Middleware` resource. Knows nothing of email addresses.

```go
Apply(ctx context.Context, cidrs []string, generation int64) error
```

`Apply` **replaces** `spec.ipAllowList.sourceRange` with exactly `cidrs`. `unionStrings` is
deleted, not repaired; its existence is the bug.

### `reconciler.go` — the glue

Two entry points share one projection step. Projecting a `*Store` to a sorted, deduplicated
CIDR slice and handing it to `Apply` is the guiding principle made concrete, and both paths
funnel through it:

```go
// project derives the desired allowlist from the store and writes it authoritatively.
func (r *Reconciler) project(ctx context.Context, s *Store) error  // -> Apply(cidrs, s.Generation)

// Authenticate is the hot path.
func (r *Reconciler) Authenticate(ctx context.Context, email, cidr string) error  // Upsert -> project

// Reconcile is the background tick: drift repair only.
func (r *Reconciler) Reconcile(ctx context.Context) error  // Load -> project
```

The background ticker survives the removal of expiry, because expiry was never its only
job. It is what repairs drift: if a middleware write is lost, or `sourceRange` is edited by
hand, the next tick re-applies the store's projection and heals it. Without it, the
allowlist could silently diverge from the store until the next authentication happened to
correct it.

### `main.go` — flags, wiring, HTTP handlers

Handlers call the reconciler and hold no state.

### Removed

- `instanceID` — the root cause; a pod hostname was never a property of the data.
- The global `users` map and `usersMu` — per-pod cached state that could diverge from the
  store.
- `unionStrings` — the append-only middleware mutation.

## Data model

One ConfigMap, one key, one JSON document.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sphinx-users
  namespace: kube-system
data:
  users.json: |
    {
      "version": 1,
      "generation": 42,
      "users": {
        "alice@example.com": {"cidr": "203.0.113.7/32",  "updatedAt": "2026-07-09T10:04:11Z"},
        "bob@example.com":   {"cidr": "198.51.100.4/32", "updatedAt": "2026-07-08T22:13:02Z"}
      }
    }
```

```go
type Record struct {
    CIDR      string    `json:"cidr"`
    UpdatedAt time.Time `json:"updatedAt"`
}

type Store struct {
    Version    int               `json:"version"`
    Generation int64             `json:"generation"`
    Users      map[string]Record `json:"users"`
}
```

Email is the map key, so a user has exactly one record by construction; re-authentication
overwrites `Record.CIDR` in place. `version` is a schema tag for future migrations.
`generation` is a monotonic counter that prevents stale middleware writes (see
Concurrency).

`UpdatedAt` records when the CIDR last *changed*, not when the user was last seen. It is
written only on a mutating `Upsert`, so it costs no extra API traffic, and it exists purely
for operator forensics — answering "when did this CIDR get registered?" during debugging.
Nothing in Sphinx reads it. A `lastSeen` field would have to be refreshed on an interval to
stay meaningful, which would defeat the write-skip cache below; `UpdatedAt` has no such
cost.

At a few thousand users this document remains well under the 1 MiB ConfigMap limit, and
the entire store is legible via `kubectl get cm sphinx-users -o yaml`.

### IP-to-CIDR conversion

The store holds a CIDR directly, replacing the `user.IP` field. Conversion moves to the
auth handler where the address is resolved, and must stop assuming IPv4: the current
`v.IP + "/32"` (`main.go:174`) is a latent bug that would silently allowlist a large range
for any client connecting over IPv6. The handler uses `/32` for IPv4 and `/128` for IPv6.

## Concurrency

Every mutating store write is a read-modify-write against the ConfigMap's
`resourceVersion`, retried on 409 Conflict. This serializes all replicas through the API
server, making `generation` strictly monotonic with no inter-pod coordination.

`generation` increments on every mutating store write, meaning an `Upsert` that adds a
record or changes an existing record's CIDR. An `Upsert` that changes nothing does not
increment it.

`generation` guards the middleware write. Given two replicas, one applying generation 42
and another 43: if 43 lands first and 42 arrives late, the late write would regress the
allowlist to an older snapshot and resurrect a just-removed CIDR. Therefore `Apply` stamps
the middleware with the annotation `sphinx.bartlett.ops/generation` and **skips any write
whose generation is strictly older than the stamped value**. Combined with the existing
`resourceVersion` retry, a stale overwrite becomes impossible rather than unlikely.

The comparison is strictly-older, not not-newer, and the distinction is load-bearing. An
`Apply` at a generation *equal* to the stamped value must proceed, because that is how the
background reconcile repairs drift — if `sourceRange` is hand-edited or a write is lost,
the next tick re-applies the same generation and heals it. Rejecting equal generations
would make the reconcile loop inert. `Apply` is idempotent, so the redundant write is
harmless. A middleware carrying no annotation is treated as generation 0, so the first
`Apply` always proceeds.

The background reconcile runs independently on every replica. It requires no leader
election, being idempotent under compare-and-swap; concurrent reconciles converge.

### The hot path

`/auth` is invoked via a Traefik `ForwardAuth` rule, so it fires on every request, not
once per login. A ConfigMap write plus a middleware write per HTTP request would overwhelm
the API server.

Each pod therefore keeps an in-memory cache of `email → cidr`. If the cached CIDR matches
the request's, the handler returns 200 without contacting Kubernetes. This is strictly a
cache and never a source of truth: a miss causes only a harmless redundant `Upsert`, which
is itself a no-op when the stored CIDR already matches, and a pod restart merely re-warms
it.

Because records never expire, the cache needs no time component and no periodic refresh — a
CIDR is either current or it is not. This is the one place the removal of expiry simplifies
rather than complicates the design: an earlier draft of this spec carried a TTL and had to
guarantee that the cache's refresh interval stayed well below it, or an actively browsing
user's record would be swept out from under them mid-session. That invariant is now gone
along with the sweep.

### Auth is synchronous

On a cache miss the handler performs `Upsert`, then `Apply`, and only then returns 201. A
201 that has not actually allowlisted the user is a lie that is difficult to diagnose from
the client.

## Configuration

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--reconcile-interval` | `SPHINX_RECONCILE_INTERVAL` | `60s` | Background drift-repair period. |

Existing flags are unchanged. One new flag, rather than three: with records persisting
indefinitely there is no `--user-ttl` and no `--refresh-interval`.

## Migration from the sharded format

On startup, if `users.json` is absent but hostname-keyed entries are present, Sphinx
migrates:

1. Merge the legacy per-pod blobs into the new document.
2. Where two blobs disagree about one user's IP, **drop that user and log the conflict at
   warning level.** The legacy format carries no timestamps, so the current CIDR cannot be
   determined; allowlisting either risks preserving a stale one, which is the bug under
   repair. The affected user re-authenticates and self-corrects.
3. Set every migrated record's `updatedAt` to the migration time, since the legacy format
   carries no timestamp to preserve.
4. Write `users.json` and delete the legacy keys in a single ConfigMap update — atomic,
   since it is one object.

The initial `Apply` that follows prunes the accumulated stale CIDRs out of the middleware.

## Error handling and observability

The auth handler returns 500 if `Upsert` or `Apply` fails. Compare-and-swap conflicts
retry with backoff up to five attempts, matching the existing cap in `updateMiddleware`
(`middleware.go:170`), before surfacing as an error.

The background reconcile loop logs and continues on error rather than crashing. A
transient API server failure must not take down the auth path; the next tick converges.

**Readiness becomes stricter.** The current `/ready` (`main.go:116`) checks only that the
middleware and ConfigMap exist, so a pod reports ready before it has ever reconciled and
can serve auth requests with an empty view of the world. The new probe additionally
requires that the initial reconcile has completed and that the last successful reconcile
occurred within `3 × reconcileInterval`. A pod that has silently lost the ability to write
the allowlist falls out of the Service.

## RBAC

ConfigMap permissions move from `get`, `patch` to `get`, `create`, `update`. Server-Side
Apply is gone, along with the per-pod field managers that existed only to coordinate the
sharded keys. Middleware permissions (`get`, `create`, `update`) are unchanged.

The README requires updating: both its RBAC section and the paragraph stating that "each
instance owns its own key in the ConfigMap (keyed by pod hostname)", which describes
precisely the design being removed.

## Testing

The `UserStore` interface makes the reconciler testable without a cluster.

Against an in-memory `UserStore` fake:

- **Regression test for the reported bug.** Authenticate `alice@example.com` from CIDR₁,
  re-authenticate from CIDR₂, assert the projected CIDR set contains CIDR₂ and not CIDR₁.
- **Pod replacement.** Write records, simulate a replica swap by constructing a fresh
  store instance against the same ConfigMap, assert no orphaned data and no growth in the
  CIDR set.
- **Persistence.** Records survive an arbitrary number of reconcile ticks unchanged; no
  code path removes a record other than an `Upsert` replacing its CIDR.
- **Generation monotonicity.** A mutating `Upsert` increments `generation`; a no-op
  `Upsert` for an unchanged CIDR leaves it untouched and writes nothing.
- **Write-skip cache.** A repeated authentication with an unchanged CIDR issues no
  Kubernetes API calls; a changed CIDR bypasses the cache and writes.

Against client-go's fake dynamic client:

- The ConfigMap store's compare-and-swap loop, with an injected 409, proving it re-reads
  and retries rather than clobbering.
- Legacy-format migration, including the conflicting-IP drop.

Against a fake allowlist writer:

- **`Apply` replaces rather than unions.** Seed `sourceRange: [stale/32]`, apply
  `[fresh/32]`, assert the result is exactly `[fresh/32]`. This single assertion would
  have caught the reported bug; `unionStrings` fails it.
- `Apply` at a generation strictly older than the middleware's annotation is a no-op.
- `Apply` at a generation *equal* to the annotation proceeds and repairs drift: seed a
  middleware stamped generation 42 whose `sourceRange` has been hand-edited, apply
  generation 42, assert `sourceRange` is corrected. This is the test that keeps the
  reconcile loop from being silently inert.
- `Apply` against a middleware carrying no generation annotation proceeds.

## Out of scope

Explicit user deletion via the API, and leader election for the reconcile loop.

**Expiry.** Records persist indefinitely by decision. The consequence is that a CIDR
registered once — from a hotel, a coffee shop, a since-abandoned home address — stays
allowlisted until that user next authenticates from somewhere else, and forever if they
never do. The allowlist therefore only grows, bounded by the number of distinct users
rather than by recent activity. Accepted deliberately: this is an allowlist of known,
authenticated users, not a session store, and the alternative imposes periodic re-auth
friction.

Reinstating expiry later is cheap and localised: add a `Sweep` method to `UserStore`, call
it from `Reconcile` before `project`, and reintroduce the write-refresh discipline that
keeps an active user's timestamp current. `UpdatedAt` is not sufficient for that on its own
— it tracks CIDR changes, not activity — so a genuine `LastSeen` field would be needed, and
with it the `refreshInterval < ttl` invariant this design was able to drop.
