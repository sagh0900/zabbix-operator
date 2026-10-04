# Operations

Day-to-day tasks for a running Zabbix system. Every change is a change to the
ZabbixSystem (or to a ConfigMap or Secret it references), so it can live in Git.

## Reading the state

```sh
kubectl -n zabbix get zsys                 # phase, active server, one-line reason
kubectl -n zabbix describe zsys zabbix     # conditions and recent events
kubectl -n zabbix get pods -L zabbix.io/component,zabbix.io/role
```

| Phase | Meaning |
|---|---|
| `Installing` | Creating or checking the schema before the first start |
| `Running` | Every component runs the desired version and template, one server node is active |
| `Upgrading` | A version change is in progress (`status.upgradeStep` for schema upgrades) |
| `Degraded` | Something is pending or unhealthy; the reason names it, for example "Waiting for database" or "Server: replacing zabbix-server-1 (template changed)" |
| `Blocked` | A requested install or upgrade cannot start; nothing was changed (see [Upgrades](upgrades.md#run-it)) |
| `Suspended` | Stopped on purpose with `spec.suspend` |

Conditions: `DatabaseReady`, `ServerActive`, `WebReady`, `Upgrading`, `UpgradeBlocked`,
`Conflict`, `UnverifiedVersion` (the release line comes from the compatibility ConfigMap) and,
with proxy registration, `ProxiesRegistered`. Events record each new active
server (`ActiveServer`), the loss of the active node (`ActiveServerLost`), upgrades, blocks
and suspends.

Every pod carries `zabbix.io/system` and `zabbix.io/component`; server pods also carry
`zabbix.io/role=active|standby`.

## Configuration changes

Change the ZabbixSystem, or a ConfigMap or Secret it references through `env`, `envFrom`
or `volumes`. Affected pods are replaced one at a time with the same names; servers go
standby first, so a server change costs one HA failover. Files mounted with `subPath` do
not update inside a running pod; the replacement picks them up.

Server tuning (pollers, caches, timeouts) is best kept in its own Secret or ConfigMap, so
it changes without touching the ZabbixSystem:

```yaml
server:
  envFrom:
    - secretRef: { name: zabbix-server-tuning }
```

Any `ZBX_*` variable of the official image works. The operator's own variables (database
connection, HA node name and address, server and web service addresses) always win.

## Scaling

| Component | Field | Notes |
|---|---|---|
| Server | `server.replicas` (1–9) | One node is active, the rest standby. Two give failover; more add nothing but standbys |
| Frontend | `web.replicas` (1–20) | Stateless; sessions live in the database |
| Web service | `webService.replicas` (1–20) | Renders scheduled reports |
| Proxy | `proxies[].replicas` (1–20) | Each instance is a separate Zabbix proxy `<name>-<i>`; use `proxyGroup` to balance hosts between them |

Scaling down removes the highest-numbered pods. The `ha_node` rows of removed servers are
deleted by `ha-gc` within about 10 minutes.

## Server failover

Zabbix decides which node is active. After a clean stop of the active node (pod deleted,
replaced, node drained) a standby takes over within about 5 seconds and the `zabbix-server`
Service follows within 2 more. After a crash or a lost node, Zabbix waits for its failover
delay (1 minute by default) before promoting a standby. Inspect and tune it inside any
server pod:

```sh
kubectl -n zabbix exec zabbix-server-0 -- zabbix_server -R ha_status
kubectl -n zabbix exec zabbix-server-0 -- zabbix_server -R ha_set_failover_delay=30s
```

A planned failover is a pod deletion: `kubectl -n zabbix delete pod <active pod>`. The
operator recreates the pod with the same name, which rejoins as standby.

## Node maintenance

`kubectl drain` evicts Zabbix pods through the eviction API; each component's
PodDisruptionBudget lets one pod go at a time, and the operator recreates evicted pods on
another node. With two servers, draining the node of the active one costs one failover.
No `--force` is needed. Agents are a DaemonSet and are skipped with
`--ignore-daemonsets`.

## Database maintenance

CNPG switchovers, failovers, minor updates and in-place major upgrades need no action on
the Zabbix side. While the `ZabbixDatabase` is not `Ready`, running servers stay up and
reconnect by themselves, the system is `Degraded` ("Waiting for database: ...") and no pod
is created, replaced or upgraded. Frequent primary changes set `PrimaryStable=False` on the
`ZabbixDatabase` (`flap.threshold` changes within `flap.window`) and hold the system until
the cluster has been quiet for one window.

If no server becomes active again after the database is back, `ServerActive` stays False
and `ZabbixServerNoActiveNode` fires; check the server logs.

## Suspend and resume

```sh
kubectl -n zabbix patch zsys zabbix --type merge -p '{"spec":{"suspend":true}}'
kubectl -n zabbix patch zsys zabbix --type merge -p '{"spec":{"suspend":false}}'
```

Suspending stops every Zabbix pod in a safe order (frontend and web service, proxies and
agents, standby servers, the active server), each server marking itself stopped in
`ha_node`. Services, configuration and status stay; the database is untouched. Use it when
Zabbix must be fully off, for example during database work that should see no clients.
Resuming starts the system like a running one. Details in
[Architecture](architecture.md#suspend).

## Proxies and agents

- In-cluster proxies send to the `zabbix-server` Service (active mode) or are reached by the
  server at `<name>-<i>.<name>.<namespace>.svc` (passive mode). Their SQLite buffer is an
  `emptyDir`; data buffered by a replaced pod is lost.
- External proxies and agents send to the address of the `zabbix-server` Service, usually a
  LoadBalancer IP (`server.service`).
- The agent DaemonSet monitors the nodes themselves under the node name as host name. Its
  image is set explicitly in `agent.image` and is not changed by Zabbix upgrades.

### Proxy registration

With `proxyRegistration.enabled`, the operator creates and updates the proxies in Zabbix:
every in-cluster proxy instance and the external proxies listed in a ConfigMap. It needs an
API token of a user allowed to manage proxies:

1. In the frontend, *Users → API tokens → Create API token* for a Super admin user.
2. `kubectl -n zabbix create secret generic zabbix-api-token --from-literal=token=<token>`
3. Enable registration (see [`examples/system-production.yaml`](../examples/system-production.yaml)).

The operator only changes proxies it was asked to manage, and deletes them only with
`prune: true`. `status.registeredProxies` lists them; the `ProxiesRegistered` condition
carries the last error. Behaviour in detail:
[Architecture](architecture.md#proxy-registration).

## The `ha_node` table

Server pods have stable names, so restarts reuse their `ha_node` rows. Rows of names that no
longer run (scaled-down servers, a previous deployment) are removed by the `ha-gc` Job every
5 minutes once they have not been seen for 2 minutes. `status.lastHANodeGCTime` shows the
last successful run; `ZabbixHANodeGCStale` fires when it stops succeeding.

## Conflicts

The operator never takes over objects it did not create. When a Pod, Service,
Ingress, PodDisruptionBudget or DaemonSet it needs already exists without its owner reference, the
system reports `Conflict=True` with the object's name, leaves it untouched and continues
with everything else. Delete or rename the other object to resolve it; the operator then
creates its own. [Migration](migration.md) uses this on purpose.

## Troubleshooting

| Symptom | Where to look |
|---|---|
| Phase `Degraded`, "Waiting for database" | `kubectl get zabbixdatabase` and its conditions; the CNPG cluster status |
| Phase `Blocked` | `phaseReason` and the Warning event; [Upgrades](upgrades.md#run-it) lists the reasons |
| "No active server node" | `zabbix_server -R ha_status` in a server pod; server logs; database reachability |
| A pod keeps being replaced | Events of the system and the pod; `zabbix_operator_pod_replacements_total{reason="failed"}` |
| Jobs fail | Failed Jobs are deleted after a minute and run again; the system's events and `zabbix_operator_job_runs_total{result="failed"}` record them, and a failed pod's termination message holds the JSON result while it exists |
| Operator errors | `kubectl -n zabbix-operator logs deployment/zabbix-operator-controller-manager` |

Every alert has a runbook entry in [Alerts](alerts.md).
