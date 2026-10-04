# Migrating from plain manifests or Helm

How to move a running Zabbix installation (Deployments or StatefulSets from plain manifests,
kustomize or a Helm chart, on a CloudNativePG database) to the operator with only one HA
failover of interruption. The database stays where it is; nothing is exported or imported.

## How it works

Zabbix HA nodes of the same version share one cluster through the `ha_node` table. A
ZabbixSystem pointed at the existing database adopts the schema and starts its server
pods, which join the running HA cluster as standbys. Removing the old servers lets one of
them take over. Until then the old Services keep their names: the operator reports them as
conflicts and leaves them alone, so clients notice nothing until the switch.

## 1. Map the existing deployment

| Existing | ZabbixSystem / ZabbixDatabase |
|---|---|
| Zabbix image tag, for example `ubuntu-7.0.25` | `spec.version: "7.0.25"`, `spec.imageFlavor: ubuntu`; use the **same** version |
| Database host, name, credentials Secret | `ZabbixDatabase`: `clusterRef`, `host`, `database`, `credentialsRef` |
| Server replicas, `ZBX_*` tuning variables, resources, TZ | `server.replicas`, `server.env` or `envFrom` (copy the variables as they are), `server.resources`, `spec.timezone` |
| `ZBX_HANODENAME` or `ZBX_AUTOHANODENAME`, `ZBX_NODEADDRESS`, `DB_SERVER_*` | Drop them; the operator sets them |
| Server Service (often a LoadBalancer with a fixed IP) | `server.service`: `type`, `loadBalancerIP`, `annotations` |
| Frontend and web service Deployments, their Services and Ingress | `web` and `webService`: `replicas`, `service`, `ingress` |
| ConfigMaps mounted into the frontend (`zabbix.conf.php`, `nginx.conf`, SAML files) | `web.volumes` and `web.volumeMounts` referencing the same ConfigMaps |
| Sidecars, for example an agent next to the server | `extraContainers` |
| `ZBX_WEBSERVICEURL`, `ZBX_SERVER_HOST` of the frontend | Drop them; the operator sets them |
| Hooks that check the database or `ha_node` | Not needed: `ZabbixDatabase` `Ready`, the `precheck` Job and the `ServerActive` condition cover them |

Name the ZabbixSystem so that its Services get the names clients already use: a system
called `zabbix` creates `zabbix-server`, `zabbix-web` and `zabbix-webservice`.

Check the existing deployment for objects with the names the operator will create (pods
`<system>-server-<i>`, `<system>-web-<i>`, PodDisruptionBudgets `<system>-<component>`). Only
the Services should collide.

Check the selectors of the existing Services too. The operator's pods carry
`app.kubernetes.io/name: zabbix`, `app.kubernetes.io/component: <component>`,
`app.kubernetes.io/instance: <system>` and `zabbix.io/*` labels. An old Service that
selects any of these alone (for example only `app.kubernetes.io/name: zabbix`) would also
send traffic to the new pods, including standby servers that do not accept connections;
add a label of the old pods to its selector before step 3.

### A previous Zabbix operator

Another operator that manages Zabbix with CRDs in the `zabbix.io` group must be removed
first: CRD names can collide (`zabbixdatabases.zabbix.io`), and two controllers must never
manage the same workloads. Removing it means an outage until the new system runs, so this
path replaces the side-by-side cutover below with a short stop:

1. Take a backup. Check that the CNPG cluster, its Pooler and the credentials Secret have no
   owner reference to the old resources, so deleting them cannot cascade into the database.
2. Scale the old operator to 0. Remove finalizers from its resources
   (`kubectl patch ... --type merge -p '{"metadata":{"finalizers":null}}'`), so nothing it
   would run on deletion fires, then delete them; their Deployments, Services and Ingresses
   go with them. A cert-manager Certificate owned by the old Ingress goes too; its Secret
   stays and can be reused by a new Certificate.
3. Uninstall the old operator (Helm does not delete CRDs it installed outside its release),
   delete its CRDs, webhooks and cluster RBAC.
4. Install this operator and create the ZabbixSystem. It adopts the existing schema; the
   measured outage on a test system was about 5 minutes.

### Agents and proxies deployed separately

- An agent DaemonSet already running on the nodes (for example the Zabbix Helm chart's) uses
  host port 10050. Remove it before enabling `agent`, or leave `agent` disabled; otherwise the
  operator's agent pods stay Pending and the system's `agent` component reports
  "unschedulable: ... didn't have free ports". Agents use the node name as host name, as the
  Helm chart's agents do, so existing hosts keep their history.
- In-cluster proxies are named `<name>-<i>`. To keep an existing proxy's hosts, rename the
  proxy in Zabbix to the new name before switching (for example `proxy.update` with the new
  `name`); proxy registration then adopts it with its host assignments.

## 2. Prepare (no impact)

1. Install the operator ([Installation](install.md#1-install-the-operator)).
2. Create the `ZabbixDatabase` for the existing CNPG cluster and wait for `Ready=True`.
3. Take a CNPG backup.
4. If the existing deployment is managed by Argo CD or Flux, stop it from reconciling the
   Zabbix workloads without deleting them (for Argo CD, disable auto-sync, or remove the
   Application with `preserveResourcesOnDeletion`), so it neither deletes the old objects nor
   restores them after the switch.

## 3. Start the operator's servers alongside

Create the ZabbixSystem with the mapped settings. The operator:

- runs `precheck`, finds the schema of the same release line and adopts it (event
  `Adopted`);
- starts its server pods; they register in `ha_node` as standby nodes next to the old ones;
- starts frontend and web service pods, which receive no traffic yet;
- reports `Conflict=True` for the Services that already exist and leaves them untouched.

Verify before going on:

```sh
kubectl -n zabbix get zsys zabbix        # Degraded, "Conflict: Service zabbix-server exists ..."
kubectl -n zabbix exec <operator server pod> -- zabbix_server -R ha_status
```

`ha_status` must list the old nodes and the operator's pods, with one node active.

## 4. Switch over

Plan a short maintenance window. Shorten the HA failover delay first, so the switch does
not wait the default minute: `zabbix_server -R ha_set_failover_delay=10s`.

1. Delete the old server Deployment or StatefulSet. Its pods stop cleanly, and one of the
   operator's standby nodes becomes active within seconds.
2. Delete the old Services that conflicted. The operator creates its own with the same
   names and settings (including a pinned LoadBalancer IP); the server Service selects the
   active node.
3. Delete the old frontend and web service Deployments and their Ingress.
4. Check: the system is `Running` with `ServerActive=True`; proxies and agents report again
   (their *last seen* times in the frontend are current); frontend login works, including
   SAML; scheduled reports render.

Proxies and agents buffer data while the server address moves, so nothing is lost; the
interruption is the failover plus the time the load balancer needs to announce the address
again.

## 5. Clean up

- `ha-gc` removes the old nodes' `ha_node` rows a few minutes after they stopped.
- Delete leftover ConfigMaps and Secrets of the old deployment that the ZabbixSystem does not
  reference.
- Restore the failover delay if you changed it.
- Remove checks and hooks of the old deployment from your pipelines.

## Rollback

- **Before step 4:** delete the ZabbixSystem. The old deployment never stopped.
- **After step 4:** delete the ZabbixSystem and re-apply the old manifests (re-enable the
  Argo CD or Flux sync). The database is unchanged: same version, no schema change.

Rehearse the whole procedure on a copy: restore the production backup into a new CNPG
cluster, deploy the old manifests unchanged against it, then migrate. Record the measured
interruption before scheduling production.
