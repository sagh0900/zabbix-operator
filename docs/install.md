# Installation

This guide installs the operator, prepares a CloudNativePG database and starts a Zabbix
system. The manifests it uses are in [`examples/`](../examples).

## Requirements

| Component | Version |
|---|---|
| Kubernetes | 1.29 or newer (CEL validation rules) |
| CloudNativePG | Any release with the `postgresql.cnpg.io/v1` API; tested with 1.30. In-place PostgreSQL major upgrades need 1.26 or newer |
| PostgreSQL | Zabbix 7.0, 7.2 and 7.4: 13 or newer. Zabbix 8.0: 15 to 18 |
| Prometheus Operator | Optional, for the monitoring bundle |

Zabbix pods run as non-root. Server and proxy containers keep the default capabilities,
because ICMP checks (`fping`) need raw sockets, which Pod Security `baseline` allows. The
agent runs in each node's network and process namespaces and needs a namespace that
allows `privileged` pods; leave it disabled otherwise.

## 1. Install the operator

Each release publishes `install.yaml` (CRDs, RBAC, the operator Deployment and its metrics
Service, in the namespace `zabbix-operator`):

```sh
VERSION=v0.1.0
kubectl apply -f https://github.com/sagh0900/zabbix-operator/releases/download/$VERSION/install.yaml
kubectl -n zabbix-operator rollout status deployment/zabbix-operator-controller-manager
```

The operator watches all namespaces. To deploy it with kustomize or Argo CD instead, use
the `config/default` base and set the image:

```yaml
# kustomization.yaml
resources:
  - https://github.com/sagh0900/zabbix-operator//config/default?ref=v0.1.0
images:
  - name: manager
    newName: ghcr.io/sagh0900/zabbix-operator
    newTag: v0.1.0
```

The operator image also runs the operator's database Jobs; the base keeps the two in sync.

## 2. Prepare the database

The operator never creates PostgreSQL clusters, roles or passwords. Create them with
CloudNativePG, then describe them with a `ZabbixDatabase`. [`examples/database.yaml`](../examples/database.yaml)
contains a namespace, the owner's credentials Secret, a three-instance CNPG `Cluster` that
creates the `zabbix` database at bootstrap, and the `ZabbixDatabase`:

```sh
kubectl apply -f examples/database.yaml        # edit the password first
kubectl -n zabbix get zabbixdatabase zabbix-db
```

```
NAME        CLUSTER     PRIMARY       READY   REASON
zabbix-db   zabbix-pg   zabbix-pg-1   True    Ready
```

Points to decide here:

- **Credentials.** `credentialsRef` names any Secret with the user and password, for example
  the `passwordSecret` of a CNPG managed role. The keys default to `username` and
  `password`.
- **TLS.** The example verifies the server with the CA CNPG keeps in `<cluster>-ca`. Remove
  `tls` to connect with libpq's default (`prefer`).
- **Pooler.** `host` may point at a CNPG `Pooler` Service in session mode. Schema work always
  uses `directHost`, which must reach the primary directly (default `<cluster>-rw`).
- **Backups.** Configure CNPG backups now. Schema upgrades wait for a recent completed
  `Backup` (see [Upgrades](upgrades.md)).
- **Shutdown time.** Zabbix keeps its database connections open, so every planned
  PostgreSQL restart waits CNPG's `smartShutdownTimeout` (180 s by default) before CNPG
  closes them. A lower value, for example 30 s, shortens CNPG maintenance; Zabbix reconnects
  by itself.

## 3. Create the Zabbix system

```sh
kubectl apply -f examples/system.yaml
kubectl -n zabbix get zsys -w
```

On an empty database the operator runs a `precheck` Job, starts one standalone server that
creates the schema, then starts the HA servers, the frontend and the web service:

```
NAME     VERSION   RUNNING   PHASE        ACTIVE            REASON
zabbix   7.0.25              Installing                     Creating the database schema on zabbix-server-init-0
zabbix   7.0.25    7.0.25    Degraded                       Server: waiting for zabbix-server-0 to become healthy
zabbix   7.0.25    7.0.25    Running      zabbix-server-0   zabbix-server-0 active, 1 standby
```

A database that already holds a Zabbix schema of the same release line is adopted, never
re-created.

[`examples/system-production.yaml`](../examples/system-production.yaml) shows the other
settings: server tuning from a Secret, a LoadBalancer Service for proxies and agents, an
Ingress for the frontend, in-cluster proxies, agents on every node and proxy registration.
Every field is described in [Architecture](architecture.md#zabbixsystem) and by
`kubectl explain zabbixsystem.spec`.

## 4. Log in

Without an Ingress, forward the frontend Service:

```sh
kubectl -n zabbix port-forward service/zabbix-web 8080:80
```

Open http://localhost:8080 and log in as `Admin` with the password `zabbix`, then change
the password. The frontend reaches the active server through the `zabbix-server` Service.

Proxies and agents send to the `zabbix-server` Service (port 10051), which always points at
the active HA node. Expose it with `server.service.type: LoadBalancer` or `NodePort` for
clients outside the cluster.

## 5. Monitoring (optional)

`monitoring.yaml` adds a ServiceMonitor, the alerting rules and the Grafana dashboard as a
ConfigMap labelled `grafana_dashboard: "1"`:

```sh
kubectl apply -f https://github.com/sagh0900/zabbix-operator/releases/download/$VERSION/monitoring.yaml
```

The metrics endpoint requires a token bound to the `zabbix-operator-metrics-reader`
ClusterRole. The bundle binds it to the ServiceAccount `prometheus-k8s` in `monitoring`;
patch the subject of the ClusterRoleBinding `zabbix-operator-metrics-reader-prometheus` for
another Prometheus (for kube-prometheus-stack: `<release>-kube-prometheus-prometheus`), and
add the labels your Prometheus selects ServiceMonitors and rules by. The
alerts and what to do about them are in [Alerts](alerts.md).

## Uninstall

Deleting a ZabbixSystem deletes everything it created: pods, the agent DaemonSet,
Services, Ingresses, PodDisruptionBudgets and Jobs. The database is not touched.

```sh
kubectl -n zabbix delete zabbixsystem zabbix
kubectl -n zabbix delete zabbixdatabase zabbix-db
kubectl delete -f https://github.com/sagh0900/zabbix-operator/releases/download/$VERSION/install.yaml
```

Deleting `install.yaml` deletes the CRDs and with them every ZabbixSystem in the cluster,
which stops all Zabbix pods. Delete the systems first, or keep the CRDs.
