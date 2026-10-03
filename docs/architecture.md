# Architecture

The Zabbix operator runs a complete Zabbix installation on Kubernetes against a PostgreSQL
database provided by [CloudNativePG](https://cloudnative-pg.io) (CNPG). It owns Zabbix
workloads only. The PostgreSQL cluster, its roles, its backups and its Poolers belong to the
user and are only *referenced*.

## Scope

| In scope | Out of scope |
|---|---|
| Zabbix server (native HA), frontend, web service, proxies, agents | Creating or changing CNPG `Cluster`, `Pooler`, `Backup`, `Database` objects |
| Schema bootstrap of an empty database, schema upgrades | Creating database roles or passwords (use CNPG `managed.roles`) |
| Patch upgrades inside an LTS line, the 7.0 → 8.0 major upgrade | Zabbix configuration objects (hosts, templates, users) |
| Cleaning stale `ha_node` rows | Certificates (Ingress TLS references an existing Secret) |
| Services and Ingresses for the components | |
| Optional registration of proxies in Zabbix | |

Supported Zabbix lines: **7.0 LTS** and **8.0 LTS**. Other lines are rejected by validation.

## Custom resources

There are exactly two CRDs, both in `zabbix.io/v1alpha1`.

### ZabbixDatabase

A thin, read-only view of a CNPG cluster that answers one question: *may Zabbix connect
right now?*

```yaml
apiVersion: zabbix.io/v1alpha1
kind: ZabbixDatabase
metadata:
  name: zabbix-db
spec:
  clusterRef:
    name: zabbix-pg            # CNPG Cluster in the same namespace
  host: zabbix-pg-rw           # default: <cluster>-rw. May point at a Pooler Service.
  directHost: zabbix-pg-rw     # host used for schema work; default: <cluster>-rw (never a Pooler)
  port: 5432
  database: zabbix
  credentialsRef:              # required; typically the secret of a CNPG managed role
    secretName: pg-zabbix-user
    usernameKey: username
    passwordKey: password
  tls:                         # optional client TLS to PostgreSQL
    mode: verify-full
    caSecretRef: { name: zabbix-pg-ca, key: ca.crt }
  flap:                        # optional primary-flap damping
    threshold: 3
    window: 10m
```

Status reports the CNPG phase, the current primary and these conditions:

| Condition | True when |
|---|---|
| `ClusterReady` | The CNPG cluster has at least one ready instance and a healthy phase |
| `CredentialsReady` | The referenced secret exists and has both keys |
| `PrimaryStable` | The primary has not changed more than `flap.threshold` times within `flap.window` (decays back to stable once quiet) |
| `Ready` | All of the above. Zabbix server pods are only started, and upgrades only run, while `Ready=True` |

The controller reads CNPG status only. It never opens a database connection itself and never
writes to CNPG objects. Every SQL statement the operator needs runs inside a short-lived Job
(see [Jobs](#jobs)).

### ZabbixSuite

Describes one Zabbix installation and owns every Kubernetes object for it.

```yaml
apiVersion: zabbix.io/v1alpha1
kind: ZabbixSuite
metadata:
  name: zabbix
spec:
  version: "7.0.25"            # Zabbix version for server, web, web service and proxies
  databaseRef:
    name: zabbix-db
  imageFlavor: ubuntu          # ubuntu | alpine → zabbix/<component>:<flavor>-<version>
  timezone: Europe/Stockholm

  upgrade:
    approveMajor: ""           # set to the target line (e.g. "8.0") to approve a major upgrade once
    requireBackupWithin: 24h   # major upgrade waits for a completed CNPG Backup this recent

  server:
    replicas: 2
    env: []                    # ZBX_* tuning passed to the image entrypoint
    service:
      type: LoadBalancer
      loadBalancerIP: 10.0.0.10
      annotations:
        metallb.universe.tf/allow-shared-ip: zabbix
    # common pod settings, see below

  web:
    replicas: 2
    service: { type: ClusterIP, port: 80 }
    ingress:
      enabled: true
      className: traefik
      annotations: {}
      hosts:
        - host: zabbix.example.com
      tls:
        - secretName: zabbix-tls
          hosts: [zabbix.example.com]

  webService:
    replicas: 1

  proxies:
    - name: proxy-dc1
      mode: active             # active | passive
      replicas: 2              # instances get hostnames proxy-dc1-0, proxy-dc1-1

  agent:
    enabled: true
    image: zabbix/zabbix-agent2:ubuntu-7.0.25

  proxyRegistration:           # optional, see "Proxy registration"
    enabled: false
```

Every component (`server`, `web`, `webService`, each proxy, `agent`) accepts the same
pod-level settings:

| Field | Purpose |
|---|---|
| `image` | Override the image derived from `version` and `imageFlavor` |
| `replicas` | Instance count (not for `agent`, which runs one pod per eligible node) |
| `resources` | Container resources |
| `env`, `envFrom` | Extra environment; operator-managed keys cannot be overridden |
| `volumes`, `volumeMounts` | Extra mounts, e.g. a patched `zabbix.conf.php` or SAML certificates from a ConfigMap |
| `extraContainers` | Sidecars, e.g. an agent2 container that monitors the server host |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | Placement |
| `podAnnotations`, `podLabels`, `priorityClassName`, `serviceAccountName`, `imagePullSecrets` | Pod metadata and identity |
| `podSecurityContext`, `securityContext` | Security settings (non-root by default) |
| `service` | `type`, `port`, `nodePort`, `loadBalancerIP`, `loadBalancerClass`, `loadBalancerSourceRanges`, `externalTrafficPolicy`, `annotations`, `labels` (not for `agent`) |
| `ingress` | `enabled`, `className`, `annotations`, `labels`, `hosts`, `tls` (`web` and `webService` only) |

Status reports `phase` (`Installing`, `Running`, `Upgrading`, `Degraded`, `Blocked`),
`runningVersion`, `activeServer` (pod name and IP of the active HA node), per-component
ready counts, and conditions `DatabaseReady`, `ServerActive`, `WebReady`, `Upgrading`,
`UpgradeBlocked`, `Conflict`.

## Workloads

The suite controller creates bare Pods for server, web, web service and proxies. It is the
only writer of those Pods and performs the work a Deployment or StatefulSet would do:
keeping the instance count, recreating lost Pods, and rolling Pods when their template
changes. Agents run as a DaemonSet, because "one pod on every eligible node" is exactly what a
DaemonSet does and reimplementing it adds risk without benefit.

### Pod identity and rollout

- Pods have stable names: `<suite>-server-0 … N-1`, `<suite>-web-0 …`,
  `<suite>-webservice-0 …`, `<proxy>-0 …`. A replaced Pod reuses its name, so Zabbix HA node
  names (and proxy hostnames) stay stable across restarts.
- Each Pod carries a hash of its rendered template. A Pod whose hash differs from the desired
  template is replaced, **one Pod at a time**, waiting for the replacement to be Running (and
  Ready, where readiness is meaningful) before touching the next.
- Server Pods are rolled standby-first: the active node is replaced last, so a configuration
  change costs one HA failover, not several.
- All Pods have an owner reference to the suite, so deleting the suite removes them.
- Because bare Pods are not owned by a controller, `kubectl drain` needs `--force` for nodes
  running them. The operator recreates evicted Pods on another eligible node. Schedule them
  with `affinity`/`topologySpreadConstraints` so replicas land on different nodes.

### Server and HA routing

The server uses Zabbix native HA. Each server Pod runs the stock image entrypoint with:

- `ZBX_HANODENAME` = Pod name (stable), `ZBX_NODEADDRESS` = Pod IP;
- database settings from the referenced `ZabbixDatabase` (host, port, name, credentials, TLS);
- the user's `env` (all `ZBX_*` tuning is honoured by the entrypoint).

Only the active HA node listens on port 10051; standby nodes run just the HA manager. The
server Pods therefore have a TCP **readiness** probe on 10051 and no liveness probe on that
port. The `<suite>-server` Service selects all server Pods, but only the active one is Ready,
so the Service, and any LoadBalancer IP in front of it, always routes to the active node.
When the active node fails or steps down, its listener closes, its readiness fails within
seconds and the new active node becomes Ready. Proxies and agents therefore always reach the
active node through one stable address.

Zabbix itself guarantees that only one node is active: nodes heartbeat through the
`ha_node` table, and an active node that loses the database for longer than the failover
delay stops processing and switches to standby. No external lease or fencing process is
needed.

### Frontend and web service

- Web Pods do **not** set `ZBX_SERVER_HOST`. The frontend reads the active node's address
  from `ha_node`, which is the only reliable way to reach the active server in HA mode.
- Web Pods are stateless (sessions live in the database) and scale horizontally behind the
  `<suite>-web` Service.
- The server is pointed at the web service with
  `ZBX_WEBSERVICEURL=http://<suite>-webservice:10053/report`.

### Proxies

Each proxy entry produces `replicas` Pods using the `zabbix-proxy-sqlite3` image with an
`emptyDir` database, so proxies are stateless. Instance `i` uses hostname `<name>-<i>`.
Active proxies send to the `<suite>-server` Service; passive proxies get a Service each.
Registering proxies and proxy groups in Zabbix is done by the user.

### Agents

When `agent.enabled` is true, a DaemonSet runs agent2 on every node matching the agent's
placement settings, with the node name as hostname and the `<suite>-server` Service as
server address. The agent image is set explicitly because agents are versioned
independently of the server.

### Exposure

Every component except the agent gets one Service, named `<suite>-server`, `<suite>-web`,
`<suite>-webservice` and `<proxy>` (passive proxies only), shaped by the component's
`service` settings. `web` and `webService` can also get an Ingress. The server has no
Ingress setting: Zabbix trapper traffic on 10051 is a raw TCP protocol, and Kubernetes
Ingress only routes HTTP. Expose it with the Service (`LoadBalancer` or `NodePort`) and
its annotations, which also covers load-balancer implementations configured that way.

Labels and annotations set by the user are merged with the operator's own; the operator
only overwrites the keys it manages (selectors and the owner labels).

### Conflicts

The operator only manages objects that carry its owner reference. If a Service or Pod with
a name it needs already exists and is not owned by the suite, the suite reports
`Conflict=True` naming the object and leaves it untouched. This makes adoption from an
existing deployment explicit and safe.

## Database availability

The operator never disrupts the database and never stops Zabbix because of it.

- While `ZabbixDatabase` is not `Ready` (CNPG switchover, failover, a PostgreSQL upgrade,
  a lost quorum), running server pods are left alone. Zabbix reconnects by itself, and
  stopping it would only lose buffered data. The suite reports `DatabaseReady=False`,
  starts nothing new and runs no upgrade step.
- When the database is back, the suite checks that a server node is active again. If
  none becomes active within a grace period, it replaces server pods one at a time,
  standby first.

### PostgreSQL version

Every Zabbix line has a minimum PostgreSQL major version (8.0 requires 15). The `precheck`
Job reads `server_version_num` from the database. If the target version is not supported:

- the upgrade does not start and the running version keeps running unchanged;
- the suite sets `UpgradeBlocked=True` with reason `PostgreSQLTooOld` and a message naming
  the current and required versions, and emits a Warning event with the same text;
- the operator re-checks periodically and after every change of the CNPG cluster. Once the
  user has upgraded PostgreSQL through CNPG, the approved upgrade continues by itself.

The same check runs at install time, so a new suite on an unsupported PostgreSQL version
is reported instead of started.

## `ha_node` maintenance

Zabbix keeps one `ha_node` row per HA node name. Becoming active or standby only changes
the row's status; it does not add rows. Because server pods have stable names, a restarted
or replaced pod reuses its own row. Rows are left behind only by names that no longer run:
scaling down, renaming the suite, or nodes from a previous deployment. The operator removes
them with a timed maintenance Job (`ha-gc`, below), and resets the table before the
standalone step of a major upgrade.

## Proxy registration

Optional, off by default. When `proxyRegistration.enabled` is true, the operator keeps the
proxies listed in a ConfigMap registered in Zabbix through the Zabbix API, using an API
token from a Secret.

```yaml
proxyRegistration:
  enabled: true
  apiTokenSecretRef: { name: zabbix-api-token, key: token }
  configMapRef: { name: zabbix-proxies, key: proxies.yaml }
  prune: false                 # true: delete proxies this suite registered that the list no longer contains
```

```yaml
# proxies.yaml
- name: proxy-dc1
  mode: active
- name: proxy-dc2
  mode: active
  proxyGroup: dc               # optional; created if missing
- name: proxy-edge
  mode: passive
  address: proxy-edge.example.com   # required for passive proxies
  port: 10051                       # optional, default 10051
```

In-cluster proxies from `spec.proxies` are registered the same way. The operator only
updates or deletes proxies it registered, recorded in the suite status, so proxies managed
by hand are never touched. Encryption settings are left to Zabbix.

## Jobs

Jobs run the operator image with a subcommand, so the project ships a single image. Each
Job has an owner reference, a deterministic name and a TTL.

| Job | When | What it does |
|---|---|---|
| `precheck` | Before any upgrade | Connects through `directHost`, reads `dbversion`, checks the target line is a valid upgrade from the database's schema, confirms no `ha_node` row is active once servers are stopped |
| `ha-reset` | Before the standalone step of a major upgrade or a fresh install | Deletes all `ha_node` rows (no server Pods exist at this point) |
| `ha-gc` | Every 5 minutes while `Running` | Deletes `ha_node` rows whose name is not a live server Pod and whose last access is older than a safety window; reports how many rows were removed |

## Lifecycle

### Fresh install

1. Wait for `ZabbixDatabase` `Ready`.
2. Start one server Pod in standalone mode (no HA node name). The image entrypoint creates
   the schema when the database is empty.
3. When that Pod is Ready, delete it, then start the configured number of HA server Pods
   one after another.
4. Start web, web service, proxies and agents.

An existing schema is detected and never re-created, so pointing a new suite at a populated
database is safe.

### Patch upgrade (7.0.x → 7.0.y, 8.0.x → 8.0.y)

Patch releases do not change the database schema, and Zabbix HA nodes are compatible
across patch versions.

1. `precheck` Job.
2. Roll server Pods one at a time, standby first, active last.
3. Roll web, web service and proxies.

Agents are not touched; their image is set separately.

### Major upgrade (7.0 → 8.0)

A major upgrade changes the schema irreversibly; the only way back is a database restore.
It runs only when `spec.upgrade.approveMajor` equals the target line, so each major
upgrade is approved explicitly and once.

1. Validate: target line is supported, PostgreSQL version satisfies the target line
   (see [PostgreSQL version](#postgresql-version)), CNPG replicas are in sync, and a CNPG
   `Backup` of the cluster completed within `requireBackupWithin`. Any failure sets
   `UpgradeBlocked=True` with the reason, emits a Warning event and changes nothing; the
   upgrade continues by itself once the condition is resolved.
2. Delete all server Pods and wait until none exist.
3. `precheck` Job, then `ha-reset` Job.
4. Start one server Pod of the new version in standalone mode. Zabbix upgrades the schema on
   start; the Pod becomes Ready once the upgrade has completed.
5. Delete the standalone Pod, then start HA server Pods one after another.
6. Roll web, web service and proxies to the new version. Proxies of the previous line
   keep sending data while they wait to be rolled.

Downgrades are rejected.

### Configuration change

A change to a component's spec changes its Pod template hash and triggers the rolling
replacement described above. A change to a referenced Secret or ConfigMap does too, through
a hash of the referenced data recorded on the Pod.

## Images

| Image | Contents |
|---|---|
| `ghcr.io/sagh0900/zabbix-operator:v<semver>` | The operator manager and the Job subcommands |

Zabbix components use the official `zabbix/*` images as published.

## Versioning and releases

The operator follows semantic versioning; `VERSION` is the single source. A `v*` tag builds
and pushes the image to ghcr.io and publishes a GitHub release with `install.yaml`
(CRDs, RBAC, Deployment).
