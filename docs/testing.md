# Testing

| Suite | Command | Runs on | Covers |
|---|---|---|---|
| Unit and envtest | `make test` | Every pull request | Builders, decisions, controllers against a real API server without kubelets |
| Database | `make test-db` | Every pull request (PostgreSQL 14, 15, 17) | The Job subcommands against real PostgreSQL |
| Monitoring | `make test-monitoring` | Every pull request | Alert rules with `promtool` fixtures |
| End-to-end | `make test-e2e` | On demand and weekly (`e2e` workflow) | Real Zabbix, PostgreSQL and CloudNativePG on kind |

## End-to-end tests

`make test-e2e` runs `hack/e2e-kind.sh`, which creates (or reuses) a kind cluster named
`zabbix-e2e`, installs CloudNativePG, builds the operator image, loads it into the nodes,
installs the operator and runs `test/e2e` (Go build tag `e2e`). It needs Docker, kind and
network access to Docker Hub and ghcr.io. The suite talks only to that cluster, through the
kubeconfig the script writes to `bin/e2e-kubeconfig`, and refuses any context that is not a
local kind cluster.

| Test | What it proves |
|---|---|
| `TestJourney/Install` | 7.4.7 installs on PostgreSQL 16; both server nodes Ready, the server Service routes only to the active one |
| `TestJourney/Failover` | Deleting the active server moves the Service to the new active node; the system returns to Running |
| `TestJourney/AgentsAndProxy` | With another agent holding host port 10050, the `agent` component reports why its pods cannot be scheduled; once it is gone, agents run on every node, and a proxy is registered through the Zabbix API and reported online and current |
| `TestJourney/SuspendAndResume` | Suspend stops every pod and leaves every `ha_node` row stopped; resume returns to Running |
| `TestJourney/UpgradeTo80` | 7.4.7 → 8.0.0rc1 (images pinned by digest): `dbversion` 7050195, the proxy current with the 8.0 server |
| `TestMigration` | A plain Deployment and Service keep serving while the system's servers join as standbys (the old Service never routes to them); removing them hands over to the system, which then creates its own Service |

Useful variables: `E2E_RUN` limits the tests (for example `E2E_RUN=TestMigration`),
`E2E_KEEP=1` keeps the test namespaces for inspection, `E2E_CLUSTER` names another kind
cluster. Delete the cluster with `kind delete cluster --name zabbix-e2e`.
