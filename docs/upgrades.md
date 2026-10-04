# Upgrades

How to move Zabbix, PostgreSQL and the operator itself to new versions. The design behind
each step is in [Architecture](architecture.md#lifecycle).

| Change | What the operator does | Zabbix interruption |
|---|---|---|
| Zabbix patch release (7.0.x → 7.0.y) | Rolls servers one at a time, standby first; then the frontend, web service and proxies | One HA failover |
| Zabbix line upgrade (7.0 → 7.4, 7.x → 8.0) | Checks, stops all servers, upgrades the schema with one standalone server, starts the HA servers, rolls the rest | All servers stopped for the schema upgrade |
| PostgreSQL minor or major release (through CNPG) | Nothing; holds rollouts while the database is not `Ready` | Zabbix reconnects by itself |
| Operator release | Rolls only pods whose rendered template changed, one at a time | At most one HA failover |

## Zabbix patch release

Set the new version:

```sh
kubectl -n zabbix patch zsys zabbix --type merge -p '{"spec":{"version":"7.0.25"}}'
kubectl -n zabbix get zsys zabbix -w
```

1. A `precheck` Job confirms the database and the schema level.
2. Server pods are replaced one at a time, standby first and the active node last, so the
   upgrade costs one HA failover.
3. Once every server runs the new version, the frontend, web service and proxies roll.

The phase is `Upgrading` until every component has moved. Agents keep their own image
(`agent.image`); change it separately.

## Zabbix line upgrade (7.x → 7.y, 7.x → 8.0)

Moving to a newer release line (7.0, 7.2, 7.4, 8.0) upgrades the database schema. Any
supported line can move directly to any newer one, for example 7.4 → 8.0; the steps are the
same for every pair, and `approveMajor` names the target line.

A schema upgrade changes the database irreversibly. The only way back is restoring a
backup, so rehearse it on a restored copy of the database first: the duration depends on
the size of the database.

### Before you start

- **PostgreSQL 15 or newer.** Zabbix 8.0 needs PostgreSQL 15 to 18. Upgrade PostgreSQL first
  (below); the operator blocks the upgrade with `PostgreSQLTooOld` until then.
- **A recent backup.** The upgrade waits for a completed CNPG `Backup` of the cluster within
  `spec.upgrade.requireBackupWithin` (default 24h). Take one right before, for example with
  `kubectl cnpg backup zabbix-pg` (add the method your cluster uses, such as
  `--method plugin --plugin-name barman-cloud.cloudnative-pg.io`).
- **Healthy replicas.** Every instance of the CNPG cluster must be healthy.
- **Proxies.** Proxies of the old line keep sending data to an 8.0 server but receive no
  configuration until they are upgraded. In-cluster proxies are rolled by the operator;
  upgrade external proxies afterwards.

### Run it

Approve the target line and set the version in one change:

```sh
kubectl -n zabbix patch zsys zabbix --type merge \
  -p '{"spec":{"version":"8.0.0","upgrade":{"approveMajor":"8.0"}}}'
kubectl -n zabbix get zsys zabbix -w
```

The operator checks, in order, and reports the first problem as phase `Blocked` with the
reason, a Warning event and the `ZabbixUpgradeBlocked` alert. Nothing stops while blocked,
and the upgrade starts by itself once the cause is gone:

| Reason | Meaning | Action |
|---|---|---|
| `PostgreSQLTooOld` | PostgreSQL is older than the target line needs | Upgrade PostgreSQL through CNPG |
| `NotPrimary` | `directHost` does not reach the primary | Point `directHost` at `<cluster>-rw` |
| `MajorUpgradeNotApproved` | `approveMajor` does not name the target line | Set `spec.upgrade.approveMajor` |
| `ReplicasNotInSync` | Not every PostgreSQL instance is healthy | Wait, or repair the CNPG cluster |
| `BackupRequired` | No completed Backup within the window | Take a backup, or set `requireBackupWithin: 0s` |
| `UnsupportedVersion` | The operator does not support the target line | Use a supported version |
| `Downgrade` | The target is older than the running version or the schema | Set a version at least as new |

Then `status.upgradeStep` shows the progress:

1. `StoppingServers`: all server pods stop. The frontend keeps running and reports the
   server as unavailable.
2. `ResettingHA`: the `ha-reset` Job clears `ha_node`.
3. `UpgradingSchema`: one standalone server of the new version upgrades the schema; a
   second `precheck` confirms the result.
4. The HA servers start on the new version, then the frontend, web service and proxies roll.
   The phase returns to `Running`.

An operator restart at any step resumes where it left off. A running schema upgrade is
never interrupted, not even by `spec.suspend`; while the database is unavailable the
upgrade pauses.

### Withdraw a request

Until the servers stop, setting `spec.version` back to the running version withdraws the
request, including a blocked one. Once the schema upgrade has started it runs to the end.

### Roll back

There is no downgrade of the schema. To return to the previous version, restore the backup
into a new CNPG cluster and start the old version on it:

1. Delete the ZabbixSystem (its pods stop; the database stays).
2. Create a CNPG cluster from the backup (`bootstrap.recovery`), for example
   `zabbix-pg-restore`.
3. Point the `ZabbixDatabase` at it: `clusterRef`, and `host`/`directHost` if set.
4. Create the ZabbixSystem again with the previous version. It finds the schema of that
   version and adopts it.

Data collected since the backup is lost.

### Release candidates and image mirrors

`spec.version` accepts release candidates such as `8.0.0rc1`, and the operator treats them
as their release line. While a version is not published under the official
`zabbix/<component>:<flavor>-<version>` tags, pin each component's image, for example by
digest, and set the same `version`:

```yaml
spec:
  version: "8.0.0rc1"
  upgrade: { approveMajor: "8.0" }
  server:
    image: zabbix/zabbix-server-pgsql@sha256:<digest>
  web:
    image: zabbix/zabbix-web-nginx-pgsql@sha256:<digest>
  webService:
    image: zabbix/zabbix-web-service@sha256:<digest>
  proxies:
    - name: proxy-k8s
      image: zabbix/zabbix-proxy-sqlite3@sha256:<digest>
```

To pull every image from a mirror, set `spec.imageRepository` (for example
`registry.example.com/zabbix`) instead.

## PostgreSQL upgrades

PostgreSQL belongs to CloudNativePG; the operator never stops Zabbix for it. During a minor
update, a switchover or a major upgrade the `ZabbixDatabase` is not `Ready`, the system
reports `Degraded` with "Waiting for database", running servers stay up and reconnect, and
nothing is rolled or upgraded until the database is back.

- **Minor release:** change `imageName` of the CNPG cluster; CNPG restarts the replicas, then
  switches over.
- **Major release:** CNPG 1.26 or newer upgrades in place when `imageName` names a newer major
  version, only to PostgreSQL 16 or newer. Take a backup first. All instances stop during the
  upgrade, and every stop waits CNPG's `smartShutdownTimeout` because Zabbix keeps its
  connections open; see [Installation](install.md#2-prepare-the-database).

A journey from PostgreSQL 14 to 8.0 therefore goes: PostgreSQL 14 → 16 or 17 in place,
Zabbix 7.0 patch upgrades as needed, then Zabbix 7.0 → 8.0.

## Operator upgrades

Apply the new release's `install.yaml` (or bump the `ref` and image tag of the kustomize
base):

```sh
VERSION=v0.1.1
kubectl apply -f https://github.com/sagh0900/zabbix-operator/releases/download/$VERSION/install.yaml
```

The new operator takes over running systems without restarting their pods. Pods are only
replaced where the new version renders a different pod template, one at a time, standby
servers first. Release notes say when that happens.
