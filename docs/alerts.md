# Alerts

Runbook for the alerts in `config/monitoring/prometheusrule.yaml`. Each alert links here
through its `runbook_url` annotation.

## ZabbixSystemRunning

Informational heartbeat, one per ZabbixSystem: fires while the system is Running and its
`ha-gc` check, which runs every 5 minutes, succeeded within the last 10 minutes. Route it to
a dead man's switch or heartbeat receiver with `repeat_interval: 5m`, so a notification
arrives after every `ha-gc` cycle:

```yaml
route:
  routes:
    - matchers: [alertname = ZabbixSystemRunning]
      receiver: heartbeat
      group_wait: 0s
      repeat_interval: 5m
```

When the notifications stop, the system left the Running phase, its maintenance stopped, or
the operator stopped reporting. It needs no action while it fires.

## ZabbixOperatorDown

No operator metrics target has been up for 5 minutes. Without the operator, Zabbix keeps
running, but lost pods are not replaced and upgrades do not progress.

1. `kubectl -n zabbix-operator get pods` and the pod's events and logs.
2. If the pod runs but is not scraped, check the ServiceMonitor labels match your
   Prometheus and that Prometheus is bound to `zabbix-operator-metrics-reader`.

## ZabbixOperatorReconcileErrors

A controller has failed reconciles for 15 minutes. The `controller` label names it.

1. Operator logs: `kubectl -n zabbix-operator logs deploy/zabbix-operator-controller-manager`.
2. Typical causes: missing RBAC after a manual change, an unreachable API server, an
   invalid object edited around validation.

## ZabbixDatabaseNotReady

The ZabbixDatabase has not been `Ready` for 5 minutes (warning) or 15 minutes (critical).
Zabbix pods keep running and reconnect by themselves, but nothing new is started and
no upgrade step runs.

1. `kubectl get zdb -n <namespace>`: the `Reason` column names the cause.
2. `ClusterNotFound`, `NoPrimary`, `PrimaryNotHealthy`, `SwitchoverInProgress`: inspect
   the CNPG cluster (`kubectl get clusters.postgresql.cnpg.io -n <namespace>`).
3. `SecretNotFound`, `KeysMissing`: the credentials Secret named in `spec.credentialsRef`.

## ZabbixDatabasePrimaryFlapping

The CNPG primary changed more often than the configured threshold within the window.
The database is reported not ready until it has been stable for a full window.

1. CNPG cluster events and instance logs: node pressure, failing storage, or network
   partitions between instances.
2. Raise `spec.flap.threshold` only if the switchovers are expected (for example a
   planned rolling maintenance).

## ZabbixServerNoActiveNode

No server Pod has been the active HA node for 2 minutes while the system runs. Proxies and
agents cannot deliver data; they buffer until a node is active again.

1. `kubectl get zsys -n <namespace>` and `kubectl get pods -l zabbix.io/component=server -L zabbix.io/role`.
2. Server logs: a node that lost the database switches to standby; a node that cannot start
   in HA mode logs why.
3. If the database is not ready, see ZabbixDatabaseNotReady first.

The alert does not fire during an upgrade, when servers are stopped on purpose.

## ZabbixServerFailoverStorm

The active server changed more than 3 times in 30 minutes.

1. Server logs around each change: database connection loss, out-of-memory kills, node
   pressure.
2. `kubectl get events -n <namespace> --field-selector involvedObject.kind=ZabbixSystem`
   lists every `ActiveServer` and `ActiveServerLost` event.

## ZabbixComponentDegraded

A component has had fewer ready Pods than desired for 10 minutes.

1. `kubectl get pods -l zabbix.io/component=<component>` and the Pod events: scheduling,
   image pulls, crash loops.
2. `kubectl get zsys -n <namespace>` shows what the rollout waits for.

## ZabbixPodReplacementsHigh

More than 5 failed Pods of one component were replaced in 30 minutes: Pods are evicted or
fail repeatedly. Check node pressure (memory, disk) and the evictions in the namespace
events.

## ZabbixUpgradeBlocked

An install or upgrade has been blocked for 30 minutes. The `reason` label and
`kubectl get zsys` say why:

- `PostgreSQLTooOld`: upgrade PostgreSQL through CNPG; the upgrade continues by itself.
- `UnsupportedVersion`: the requested Zabbix line is not supported by this operator version.
- `Downgrade`: the database schema is newer than the requested version; set the version
  back.
- `NotPrimary`: `directHost` of the ZabbixDatabase does not reach the primary.

## ZabbixUpgradeStuck

A version change has run for more than 2 hours. Schema upgrades of large databases can take
long; `kubectl get zsys` shows the current step, and the standalone server's log shows the
schema upgrade progress in percent.

## ZabbixOperatorJobFailing

A database Job (`precheck`, `ha-reset`, `ha-gc`) failed at least twice in an hour.
`kubectl logs job/<name>` shows the JSON result with the reason; typical causes are database
credentials, TLS settings or network access to `directHost`.

## ZabbixHANodeGCStale

`ha-gc` has not succeeded for 30 minutes while the system runs, so rows of removed server
Pods may stay in `ha_node`. Check the latest `ha-gc` Job and its result.

## ZabbixAgentNodesMissing

The agent DaemonSet has been ready on fewer nodes than it should run on for 15 minutes.

1. `kubectl get daemonset <system>-agent -n <namespace>` and the events of its pods.
2. Agents run with the node's network and process namespaces: the namespace must allow
   that (Pod Security `privileged`), and the agent port 10050 must be free on every node.
3. Nodes the agent should not run on are excluded with `agent.nodeSelector`,
   `agent.affinity` or missing `agent.tolerations`.

## ZabbixProxyRegistrationFailing

The last proxy registration sync failed and has kept failing for 15 minutes. Running proxies
keep working; new or changed proxies are not registered until it succeeds.

1. `kubectl get zabbixsystem <system> -n <namespace> -o yaml`: the `ProxiesRegistered`
   condition and the `ProxyRegistrationFailed` events carry the Zabbix API error.
2. `InvalidConfiguration`: fix the proxy list in the ConfigMap or the token Secret.
3. An authorization error: the API token in `proxyRegistration.apiTokenSecretRef` expired,
   was revoked, or its user lacks the permission to manage proxies (Super admin role).
4. A connection error: the frontend is not ready, or `proxyRegistration.url` is wrong.
5. Deleting a proxy that still monitors hosts fails; move those hosts first.

