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
| `PostgreSQLTooNew` | PostgreSQL is newer than the target line supports | Use a supported PostgreSQL version, or extend the line's range (see [Adding a release line](#adding-a-release-line)) |
| `ReplicasNotInSync` | Not every PostgreSQL instance is healthy | Wait, or repair the CNPG cluster |
| `BackupRequired` | No completed Backup within the window | Take a backup, or set `requireBackupWithin: 0s` |
| `UnsupportedVersion` | The target line is neither built in nor in the compatibility ConfigMap | Use a supported version, or [add the line](#adding-a-release-line) |
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

## Supported versions

The operator runs the release lines built into it, each with the PostgreSQL range of its
official requirements:

| Line | PostgreSQL |
|---|---|
| 7.0 | 13 to 18 |
| 7.2 | 13 or newer |
| 7.4 | 13 to 18 |
| 8.0 | 15 to 18 |

These versions and paths have been run end to end on a live cluster (CloudNativePG 1.30)
with this operator version:

| Version or path | PostgreSQL | Result |
|---|---|---|
| Install 7.0.1 | 14 | Running in 44 s |
| 7.0.1 → 7.0.25 (patch) | 16 | 32 s, servers standby first, no restarts |
| Install 7.0.25 | 17 | Running in 31 s, TLS `verify-full` |
| 7.0.25 → 8.0.0rc1 (line upgrade) | 17 | 28 s, server Service empty for about 13 s |
| Install 7.4.7 | 16 | Running in 70 s |
| 7.4.7 → 8.0.0rc1 (line upgrade) | 16 | 26 s, no restarts |
| PostgreSQL 14 → 16 → 17 in place under running Zabbix 7.0 | 14 to 17 | Zabbix reconnects, never stopped |

Other patch releases of these lines work the same way: they share the line's schema.

### Adding a release line

A new Zabbix line can be enabled without a new operator version, through the optional
ConfigMap `zabbix-operator-compatibility` in the operator's namespace. Its lines are
added to the built-in ones; an entry for a built-in line replaces its PostgreSQL range.

1. Check the version against what Zabbix publishes. From a clone of this repository:

   ```sh
   hack/zabbix-line-check.sh 8.2.0
   ```

   The script confirms that every official image is published, reads the schema version
   the server ships, takes the PostgreSQL range from the official requirements page, and
   prints the ConfigMap entry. It changes nothing.

2. Read the line's upgrade notes in the Zabbix documentation for changes in images,
   configuration variables, HA behaviour and the API.

3. Create the ConfigMap:

   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: zabbix-operator-compatibility
     namespace: zabbix-operator
   data:
     compatibility.yaml: |
       lines:
         - line: "8.2"
           minPostgres: 15
           maxPostgres: 18   # optional
   ```

   The operator reads it within 30 seconds. An unreadable or invalid ConfigMap is
   ignored (only the built-in lines apply) and fires `ZabbixOperatorCompatibilityConfigInvalid`.

**A line from the ConfigMap has not been validated with this operator version.** A new
Zabbix line can change its images, its configuration variables, its HA behaviour or its
API, and an upgrade to it rewrites the database schema, which only a restore undoes.
Therefore:

- Every system whose desired or running line comes from the ConfigMap reports
  `UnverifiedVersion=True` and a Warning event.
- An upgrade to such a line always waits for a completed CNPG `Backup` (within
  `requireBackupWithin`, or 24 hours when that check is turned off).
- Rehearse the upgrade on a restored copy of the database before upgrading a system that
  matters, and check the frontend, proxies, agents and failover afterwards.

A later operator release that validates the line builds it in; the ConfigMap entry can
then be removed.

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
