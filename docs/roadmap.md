# Roadmap to v0.1.0

The work to reach [architecture.md](architecture.md), split into pull requests that each
build, pass tests and leave `main` releasable. Remove this file once v0.1.0 is released.

## Pull requests

| # | Branch | Content |
|---|---|---|
| 1 | `ci/pipeline` | GitHub Actions: lint, tests and a multi-arch image build on every pull request; on a `v*` tag, push the image to ghcr.io and publish a GitHub release |
| 2 | `feat/database` | `ZabbixDatabase` API and controller, envtest CNPG CRDs, `config/` manifests, `install.yaml` attached to releases |
| 3 | `feat/metrics` | Secure metrics endpoint and Service, database metrics, `config/monitoring` (ServiceMonitor, PrometheusRule, dashboard), `monitoring.yaml` and `dashboard.json` release assets |
| 4 | `feat/suite-api` | `ZabbixSuite` API: shared pod, service and ingress settings, CEL validation (version pattern, supported lines, no downgrade) |
| 5 | `feat/podset` | `internal/podset`: render, hash, create, replace one at a time, recreate lost pods, conflict detection |
| 6 | `feat/jobs` | Job subcommands `precheck`, `ha-reset`, `ha-gc` in the manager binary |
| 7 | `feat/suite-install` | Suite controller: fresh install, server HA with readiness routing, web, web service, Services, Ingresses, `ha_node` GC |
| 8 | `feat/upgrades` | Patch and major upgrades, PostgreSQL version gate, backup gate, database-availability handling |
| 9 | `feat/proxies-agents` | In-cluster proxies, agent DaemonSet |
| 10 | `feat/proxy-registration` | Zabbix API client and optional proxy registration from a ConfigMap |
| 11 | `docs/operations` | Install guide, examples, operations and upgrade guides, migration guide from plain manifests |
| 12 | `test/e2e` | Live-cluster suites below |

From PR 3 on, every pull request that adds behaviour also adds its metrics, alerts and
dashboard panels from [Observability](architecture.md#observability).

Release **v0.1.0** after PR 12.

## Production-readiness tests

**Unit (every pull request)**
- Pod template hashing, rollout order (standby first, active last), replica computation.
- Upgrade path validation: patch, major with and without approval, unsupported line,
  downgrade, PostgreSQL too old.
- Flap damping including the return to stable.
- Env, label and annotation merge with operator-managed keys.
- Proxy definition parsing and the create/update/prune decision.

**envtest (every pull request)**
- Fresh install: standalone first, then HA scale-out.
- Lost pod recreated with the same name; template change rolls one pod at a time.
- Conflict with a pre-existing Service or Pod not owned by the suite.
- Database not ready: nothing stops, nothing starts; recovery without manual action.
- Upgrade blocked by PostgreSQL version, then resumed after the version changes.
- Upgrade state survives an operator restart at every phase.

**Monitoring (every pull request that touches metrics)**
- Rules pass `promtool check rules` and `promtool test rules` with fixtures for every alert.
- The dashboard JSON is valid and every query references an exported metric.

**Live cluster (before every release)**
- HA failover: delete the active server pod; time until the Service routes to the new
  active node; proxies and agents keep delivering data.
- Active node loses database connectivity: it steps down, never two active nodes, the
  Service follows.
- CNPG switchover, failover and a PostgreSQL major upgrade, in steady state and during a
  Zabbix upgrade.
- Patch upgrade 7.0.x → 7.0.y under load; measured data gap.
- Major upgrade 7.0 → 8.0 on a copy of production data: duration, every blocked reason,
  and recovery by restoring the backup. Until 8.0 images are published under release tags,
  this runs on `zabbix/*:ubuntu-trunk` pinned by digest (currently 8.0.0rc1).
- Node drain and node loss for every component.
- Operator restart and operator upgrade during a rollout.
- `ha_node` GC: stale rows removed, live rows never removed.
- Proxy registration: create, update, prune; hand-made proxies untouched.
- Migration from an existing plain-manifest deployment, end to end.
- Alerts: each alert fires in its failure scenario above and resolves afterwards.
- Soak: 72 hours steady state; no pod restarts, no `ha_node` growth, flat operator memory.
