# OVHcloud IP Load Balancer Operator

A Kubernetes operator (Go, built with [kubebuilder](https://book.kubebuilder.io)
and controller-runtime) that manages the configuration of an **existing**
OVHcloud [IP Load Balancer](https://www.ovhcloud.com/en/network/load-balancer/)
service declaratively, through the OVHcloud public API.

It manages three resources as Custom Resources:

| Kind          | API group / version            | Manages |
| ------------- | ------------------------------ | ------- |
| `Farm`        | `iplb.ovhcloud.com/v1alpha1`   | Server clusters (backend pools) and their member servers |
| `Frontend`    | `iplb.ovhcloud.com/v1alpha1`   | Listeners (port, default farm, allowed sources, TLS) |
| `Certificate` | `iplb.ovhcloud.com/v1alpha1`   | SSL certificates uploaded from a Kubernetes TLS Secret |

It can also act as a **Service load-balancer controller**: it watches core
Kubernetes `Service` objects of `type: LoadBalancer` that opt in via
`spec.loadBalancerClass: iplb.ovhcloud.com/iplb`, and translates each into the
`Farm`/`Frontend` resources above (see [below](#service-of-type-loadbalancer)).

It does **not** order or provision new IPLB services — you bring an existing
`serviceName` (e.g. `loadbalancer-xxxxxxxx`).

## How it works

The OVHcloud IPLB API is a **two-phase / staged** API: every write to a farm,
frontend, server or certificate is *staged*, not live. Nothing takes effect
until `POST /ipLoadbalancing/{serviceName}/refresh` ("apply"). The set of staged
changes is visible through `GET /ipLoadbalancing/{serviceName}/pendingChanges`.

Each controller:

1. Resolves the `serviceRef` (service name + OAuth2 credentials Secret) into an
   authenticated client.
2. Reconciles the live API object to match the spec (create / adopt-by-name /
   update / delete server members), storing the OVHcloud-assigned numeric id in
   `.status`.
3. After staging a change, signals a shared **refresh coordinator** and marks
   the resource `Ready=False` with reason `PendingRefresh`.

The **refresh coordinator** ([internal/refresh](internal/refresh/coordinator.go))
debounces refresh requests **per service** (default 5s of quiescence), so a
burst of changes across many resources collapses into a single `refresh`. It
only calls `refresh` when `pendingChanges` reports outstanding work, and it
runs under leader election so multiple replicas never apply concurrently. Once
the service has no pending changes, resources flip to `Ready=True` / `Applied`.

## Repository layout

```
api/v1alpha1/            CRD Go types (Farm, Frontend, Certificate, shared ServiceRef)
internal/ovhclient/      OAuth2 client factory (reads credentials Secret, caches clients)
internal/iplb/           Typed wrappers over /ipLoadbalancing/... endpoints
internal/refresh/        Per-service debounced refresh coordinator
internal/controller/     The reconcilers (Farm, Frontend, Certificate, Service)
internal/fakeovh/        In-memory fake of the IPLB API used by tests
config/                  Kustomize manifests (CRDs, RBAC, manager, samples)
cmd/main.go              Manager entrypoint and wiring
```

## Prerequisites

- Go 1.26+
- An OVHcloud IP Load Balancer service and its `serviceName`.
- An OVHcloud **OAuth2 service account** (client id + client secret) with access
  to the IP Load Balancing API (see below).
- A Kubernetes cluster and `kubectl`.

## Credentials: create an OAuth2 service account

The operator authenticates with OAuth2 `client_credentials` via
[`go-ovh`](https://github.com/ovh/go-ovh).

1. Create a service account / OAuth2 client in the OVHcloud Manager
   (*Identity and Access Management → Service accounts*), or via the API. The
   `CLIENT_CREDENTIALS` flow needs no callback URL.
2. Grant it access to the IP Load Balancing resources (an IAM policy covering
   the `GET/POST/PUT/DELETE` actions on your `/ipLoadbalancing/*` service).
3. Note the generated **client ID** and **client secret**.

Create the Kubernetes Secret (keys `clientID` and `clientSecret`):

```sh
kubectl create secret generic ovh-credentials \
  --from-literal=clientID=<your-client-id> \
  --from-literal=clientSecret=<your-client-secret>
```

The `endpoint` field on `serviceRef` selects the region (`ovh-eu` default,
`ovh-ca`, `ovh-us`).

## Build, test, run

> **Note on paths with spaces:** the bundled `Makefile` (and the golangci-lint
> plugin build) do not cope with spaces in the project path. The Go toolchain
> itself is fine, so use the direct commands below, or clone into a path without
> spaces to use `make` normally.

```sh
# Build and test
go build ./...
go test ./...

# Regenerate DeepCopy + CRD/RBAC manifests after changing api/ types
./bin/controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."
./bin/controller-gen rbac:roleName=manager-role crd webhook paths="./..." \
    output:crd:artifacts:config=config/crd/bases

# Install the CRDs into the current cluster
kubectl apply -f config/crd/bases/

# Run the controller locally against your kubeconfig
go run ./cmd/main.go --refresh-debounce=5s
```

Flags of note: `--refresh-debounce` (coalescing window, default 5s),
`--leader-elect`, `--metrics-bind-address`, `--health-probe-bind-address`.

## Install with Helm

A chart is generated by the kubebuilder `helm/v2-alpha` plugin at
[dist/chart/](dist/chart/). It installs the CRDs, RBAC, ServiceAccount and the
controller Deployment.

```sh
helm install iplb-operator ./dist/chart \
  --namespace iplb-system --create-namespace \
  --set manager.image.repository=<registry>/iplb-operator \
  --set manager.image.tag=<version>
```

Useful values (see [dist/chart/values.yaml](dist/chart/values.yaml) for the full
list):

| Value | Default | Purpose |
| ----- | ------- | ------- |
| `manager.image.repository` / `manager.image.tag` | `controller` / chart appVersion | Controller image |
| `manager.replicas` | `1` | Manager replicas (use with leader election for HA) |
| `manager.args` | `[--leader-elect]` | Manager flags (add `--refresh-debounce=...` here) |
| `rbac.namespaced` | `false` | ClusterRole/Binding (false) vs namespace-scoped Role |
| `metrics.enabled` / `metrics.secure` | `true` / `true` | Expose the metrics endpoint (HTTPS + authn/authz when secure) |
| `metrics.reader.enabled` | `false` | Bind a ServiceAccount to the `metrics-reader` ClusterRole so it can scrape `/metrics` |
| `metrics.reader.serviceAccounts` | `[]` (operator's own SA) | List of `{name, namespace}` granted read access to `/metrics` |
| `prometheus.enabled` | `false` | Create a `ServiceMonitor` for the metrics endpoint |
| `prometheusRule.enabled` | `false` | Create a `PrometheusRule` with alerting rules (set `prometheusRule.additionalLabels` to match your Prometheus `ruleSelector`) |
| `grafanaDashboard.enabled` | `false` | Ship the Grafana dashboard as a sidecar-discovered ConfigMap |
| `crd.enabled` / `crd.keep` | `true` / `true` | Install CRDs / keep them on uninstall |

The CRDs are templated under `templates/crd/` (so `helm upgrade` applies schema
changes) and carry `helm.sh/resource-policy: keep` while `crd.keep=true`, so the
custom resources survive an uninstall. The Makefile also has `helm-deploy`,
`helm-uninstall`, `helm-status`, `helm-history` and `helm-rollback` targets.

### Monitoring

The controller exposes the standard controller-runtime metrics on the secure
`/metrics` endpoint (reconcile rate/latency/errors, work-queue depth/latency,
Kubernetes API client calls, leader status, Go runtime). To scrape with
Prometheus and visualise in Grafana:

- `prometheus.enabled=true` creates a `ServiceMonitor`.
- `metrics.reader.serviceAccounts` binds your Prometheus ServiceAccount to the
  `metrics-reader` ClusterRole so its scrape token is authorized.
- `prometheusRule.enabled=true` creates a `PrometheusRule` with alerts
  (operator down, no leader, reconcile errors, work-queue backlog, slow
  reconciles, restarts). Set `prometheusRule.additionalLabels` so the rule
  matches your Prometheus `ruleSelector` (e.g. `release: <kube-prometheus-stack>`).
- `grafanaDashboard.enabled=true` ships the dashboard at
  [config/grafana/iplb-operator-dashboard.json](config/grafana/iplb-operator-dashboard.json)
  as a ConfigMap labelled `grafana_dashboard: "1"` (auto-loaded by the
  kube-prometheus-stack Grafana sidecar). You can also import that JSON manually.

See [dist/chart/values-prod.yaml](dist/chart/values-prod.yaml) for a complete
example wiring all three.

> Regenerate the chart after changing CRDs, RBAC markers or the manager spec:
> `kubebuilder edit --plugins=helm/v2-alpha --force` (back up any custom
> `values.yaml`/`manager.yaml` edits first).

## Usage

1. Create the credentials Secret (above).
2. Apply a `Farm`, then a `Frontend` that references it, and optionally a
   `Certificate`. See [config/samples/](config/samples/).

```sh
kubectl apply -f config/samples/iplb_v1alpha1_farm.yaml
kubectl apply -f config/samples/iplb_v1alpha1_frontend.yaml
kubectl get farms,frontends,certificates
```

```
NAME                            SERVICE                  PROTOCOL   FARMID   READY
farm.iplb.ovhcloud.com/web      loadbalancer-xxxxxxxx    http       12345    True
```

### Farm (with embedded servers)

Servers are a declarative list inside the `Farm`. The operator creates, updates
and deletes server members to match the list (keyed by `address`):

```yaml
apiVersion: iplb.ovhcloud.com/v1alpha1
kind: Farm
metadata:
  name: web
spec:
  serviceRef:
    serviceName: loadbalancer-xxxxxxxx
    credentialsSecretRef: {name: ovh-credentials}
  protocol: http
  balance: roundrobin
  servers:
    - {address: 10.0.0.11, port: 8080}
    - {address: 10.0.0.12, port: 8080}
```

### Frontend (referencing a Farm)

`defaultFarmRef` points at a `Farm` custom resource in the same namespace; the
operator waits until that Farm has a `farmId` and uses it as the default farm.
Use `defaultFarmId` instead to target a raw OVHcloud farm id.

`protocol` may be `http`, `tcp` or `udp`. UDP frontends/farms support only a
minimal field set (no TLS, allowedSource/deniedSource, stickiness, probe, weight
or HTTP routes); the operator silently omits those for `udp`.

**IP ACLs**: `allowedSource` restricts inbound traffic to those CIDRs (allow-list)
and `deniedSource` blocks the given CIDRs (deny-list). They are **mutually
exclusive** — the OVHcloud API rejects a frontend with both, so the operator
refuses the combination up front with an `InvalidConfiguration` Event and leaves
the resource `Ready=False` instead of looping on a remote 400. From a Service,
`loadBalancerSourceRanges` feeds `allowedSource` and the
`iplb.ovhcloud.com/denied-source` annotation feeds `deniedSource`; set only one.

**TLS** (http frontends): set `ssl: true`, or reference a `Certificate` CR with
`defaultCertificateRef` (or a raw `defaultSslId`) — a referenced certificate
implies `ssl: true`. `forceSni`, `hsts` and `redirectLocation` are also
available.

**HTTP routes** (http frontends): `spec.routes` is a list of `{name, action,
rules, weight}`. Each route runs its `action` (`farm` — via `targetFarmRef` or a
raw `target` id; `redirect` with a URL `target` and `status`; or `reject`) when
all its `rules` match. A rule is `{field, match, pattern, subField, negate}`,
where `match` is one of `contains`/`endswith`/`exists`/`in`/`internal`/`is`/
`matches`/`startswith`. The operator reconciles routes and their rules and
deletes routes dropped from the spec.

```yaml
spec:
  protocol: http
  port: "80"
  defaultFarmRef: web
  routes:
    - name: redirect-to-https
      action: {type: redirect, target: "https://${host}${path}${arguments}", status: 302}
      rules:
        - {field: path, match: startswith, pattern: /}
```

### Certificate

A `Certificate` has two mutually exclusive modes:

- **Upload from a Secret** — `tlsSecretRef` points at a standard
  `kubernetes.io/tls` Secret. Because the SSL API has no in-place update of
  certificate material, rotating the Secret content triggers a *replace*
  (delete-then-recreate) on the load balancer.
- **Free Let's Encrypt** — `freeCertificate.fqdn` lists the domain(s) to certify.
  The operator calls `POST .../freeCertificate`, then adopts the issued
  certificate once OVHcloud has completed ACME validation (the certificate is
  auto-renewed by OVHcloud). The FQDN(s) **must resolve to the load balancer** or
  validation never completes. Issuance is asynchronous: the `Certificate` stays
  `Ready=False`, reason `Pending`, until the certificate appears, then flips to
  `Ready=True`.

```yaml
apiVersion: iplb.ovhcloud.com/v1alpha1
kind: Certificate
metadata: {name: app-cert}
spec:
  serviceRef:
    serviceName: loadbalancer-xxxxxxxx
    credentialsSecretRef: {name: ovh-credentials}
  freeCertificate:
    fqdn: [app.example.com]
```

A `Frontend` then serves it via `defaultCertificateRef: app-cert`.

### Service of type LoadBalancer

The operator can manage the IPLB from plain Kubernetes `Service` objects, the
way a cloud-provider load-balancer controller does. A Service opts in with
`spec.loadBalancerClass: iplb.ovhcloud.com/iplb` and points at an IPLB service
through annotations:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: web
  annotations:
    iplb.ovhcloud.com/service-name: loadbalancer-xxxxxxxx   # required
    iplb.ovhcloud.com/credentials-secret: ovh-credentials   # required
spec:
  type: LoadBalancer
  loadBalancerClass: iplb.ovhcloud.com/iplb
  selector: {app: web}
  ports:
    - {name: http, port: 80, targetPort: 8080, protocol: TCP}
```

For each **TCP** port the operator creates an owned `Farm` + `Frontend` pair
(named `<service>-<port>`) in the Service's namespace:

- the **Farm**'s servers are the cluster **node IPs** (Internal by default),
  each reached on the Service's allocated **`nodePort`** — so the external IPLB
  forwards to `nodeIP:nodePort` and kube-proxy routes on to the pods;
- the **Frontend** listens on the Service `port` and targets that Farm;
- `spec.loadBalancerSourceRanges` becomes the frontend's `allowedSource`;
- the IPLB's public IP is written back to `status.loadBalancer.ingress`.

The Farm/Frontend reconcilers then drive the OVHcloud API and the shared refresh
exactly as for hand-written CRs. Removing a port, or the Service, garbage-collects
the owned resources. Adding or draining nodes re-reconciles farm membership.

#### Supported annotations

All annotations are optional unless marked **required**. They are grouped below by
what they configure; every annotation is prefixed with `iplb.ovhcloud.com/`.

##### Service binding (required)

| Annotation | Default | Purpose |
| ---------- | ------- | ------- |
| `service-name` | — (**required**) | Target IPLB `serviceName` (e.g. `loadbalancer-xxxxxxxx`) |
| `credentials-secret` | — (**required**) | Name of the OAuth2 credentials Secret (keys `clientID`, `clientSecret`) |
| `credentials-namespace` | Service namespace | Namespace of the credentials Secret |
| `endpoint` | `ovh-eu` | API region: `ovh-eu`, `ovh-ca` or `ovh-us` |

##### Farm (backend pool)

| Annotation | Default | Purpose |
| ---------- | ------- | ------- |
| `protocol` | per-port (`udp` for UDP ports, `http` for an `http(s)` appProtocol, else `tcp`) | Force the farm/frontend protocol |
| `zone` | auto when the LB has exactly one zone | Balancing zone for the farms/frontends |
| `balance` | `roundrobin` | Load-balancing algorithm (`roundrobin`, `leastconn`, `source`, …) |
| `stickiness` | from `sessionAffinity` (`ClientIP`→`sourceIp`) | Session persistence: `sourceIp` or `cookie` |
| `vrack-network-id` | auto when the LB has exactly one vRack network | vRack private network id the farms attach to |

##### Backend servers

| Annotation | Default | Purpose |
| ---------- | ------- | ------- |
| `node-address-type` | `InternalIP` | Node address used as the backend: `InternalIP` or `ExternalIP` |
| `server-ssl` | — | TLS from the load balancer to the backends (bool) |
| `server-backup` | — | Mark backends as backup-only (bool) |
| `server-weight` | — | Relative weight of each backend (0-256) |
| `server-health-check` | `true` when a probe is configured | Enable the farm probe on each backend (bool) |

##### Health check (farm probe)

Setting any of these enables the probe:

| Annotation | Default | Purpose |
| ---------- | ------- | ------- |
| `health-check-protocol` | — (probe disabled) | Probe type: `tcp`, `http`, `https`, … |
| `health-check-port` | backend (node) port | Port the probe targets |
| `health-check-interval` | API default | Probe interval, seconds |
| `health-check-url` | — | Request path for `http`/`https` probes |
| `health-check-method` | API default | HTTP method for `http`/`https` probes |

##### Frontend (TLS & access control)

http frontends only:

| Annotation | Default | Purpose |
| ---------- | ------- | ------- |
| `tls-certificate-ref` | — | Certificate CR served as the default TLS cert (enables SSL) |
| `tls-free-certificate` | — | `"true"` requests a free Let's Encrypt cert for `tls-host` |
| `tls-host` | — | Comma-separated FQDN(s) for the free certificate (must resolve to the LB) |
| `denied-source` | — | Comma-separated CIDRs to block on the frontends (deny-list) |

(The Service's own `spec.loadBalancerSourceRanges` feeds the frontend allow-list.)

##### Status

| Annotation | Default | Purpose |
| ---------- | ------- | ------- |
| `ip` | — | Override the IP written to `status.loadBalancer.ingress` instead of querying the API |

TCP and UDP ports are handled (UDP ports map to `udp` farms/frontends); other
protocols (e.g. SCTP) emit a warning Event and are skipped. To terminate TLS,
add a `Certificate` (see above) and point `iplb.ovhcloud.com/tls-certificate-ref`
at it; the http frontend then gets `ssl: true` and that certificate as default.
For a managed Let's Encrypt certificate, instead set
`iplb.ovhcloud.com/tls-free-certificate: "true"` and
`iplb.ovhcloud.com/tls-host: app.example.com` — the operator creates an owned
free `Certificate` for those FQDN(s) and wires it into the http frontends (the
host must resolve to the load balancer for issuance to succeed).
The zone is taken from the LB automatically when it advertises exactly one; if it
has several, the operator emits a `ZoneRequired` Event and waits until you set the
`iplb.ovhcloud.com/zone` annotation.

**vRack (private) load balancers.** When the IPLB is attached to a vRack, farms
must reference a vRack network id. The operator auto-discovers it when the LB has
exactly one vRack network; with several it emits a `VrackNetworkRequired` Event
and waits for the `iplb.ovhcloud.com/vrack-network-id` annotation. The vRack
network must already exist on the load balancer (create it in the Manager or via
`POST /ipLoadbalancing/{serviceName}/vrack/network`) and its subnet must cover the
node addresses used as backends (the node `InternalIP`s, which are the vRack
private IPs on an OVHcloud Managed Kubernetes cluster in private mode).

## Status & conditions

Each resource exposes a `Ready` condition:

- `Ready=False`, reason `PendingRefresh` — changes staged, waiting for the
  service-wide apply.
- `Ready=False`, reason `DependencyNotReady` — a `Frontend` is waiting on its
  referenced `Farm`.
- `Ready=False`, reason `Error` — see the condition message.
- `Ready=True`, reason `Applied` — configuration is live (no pending changes).

When the OVHcloud API rejects a request with a 4xx (bad zone, missing IAM
permission, unknown service, …), the operator treats it as a *terminal*
configuration error: it records the message on the `Ready=False`/`Error`
condition, emits a `ConfigurationError` Event (visible in `kubectl describe`),
and retries slowly (every 2 minutes) instead of hot-looping. Transient errors
(429, 5xx) keep their normal fast exponential backoff.

## Design notes & limitations

- **Adoption:** a resource with no stored id first tries to adopt a live object
  with the same `displayName` before creating a new one — safe to point the
  operator at a partially pre-configured service.
- **Drift detection** compares only fields the spec sets explicitly, so the
  operator never fights OVHcloud's own defaults for unset fields. The farm
  **probe** is an exception: it is fully reconciled, so removing it from the spec
  (or the Service health-check annotations) actively clears the probe on the load
  balancer rather than leaving the previous one in place.
- `v1alpha1` models the common farm/frontend fields plus http frontend TLS
  (`ssl`, `defaultSslId`/`defaultCertificateRef`, `forceSni`, `hsts`,
  `redirectLocation`) and HTTP routes. Remaining protocol-specific extras (custom
  HTTP headers, `dedicatedIpfo`, …) are future work.
- UDP is supported with the reduced field set the OVHcloud API allows for it. A
  UDP `Farm` requires an explicit `spec.port` (the API rejects creation without
  it); the Service-driven flow sets it to the backend nodePort automatically.
- **Refresh requires a frontend.** The OVHcloud API rejects a refresh when the
  service has no frontend ("Can not refresh: this Load Balancer has no Frontend").
  The operator treats this (and other 4xx refresh errors) as "not applyable yet":
  it does not hot-loop, and applies the staged configuration once a frontend
  exists. A `Farm`/`Certificate` therefore only goes `Ready` once a `Frontend`
  is present on the same service.
- **Self-healing:** if a stored farm/frontend/route id 404s on the API (e.g. a
  staged-but-unapplied object was discarded), the operator forgets the id and
  recreates the object; deletes are idempotent, so finalizers complete even when
  the object is already gone.
- Servers are embedded in the `Farm`. A standalone `Server` CRD could be added
  later for high-churn, per-pod backend registration.

## Testing

Tests are self-contained: they use controller-runtime's fake client and an
in-memory fake of the IPLB API ([internal/fakeovh](internal/fakeovh/fakeovh.go)),
so no cluster, envtest binaries, or OVHcloud account is required.

```sh
go test ./...
```
