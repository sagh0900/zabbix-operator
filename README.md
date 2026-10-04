# Zabbix Operator

A Kubernetes operator that runs Zabbix (server in native HA, frontend, web service,
proxies, agents) on a PostgreSQL database managed by
[CloudNativePG](https://cloudnative-pg.io).

- Two custom resources: `ZabbixDatabase` and `ZabbixSystem`.
- Zabbix 7.0 LTS and 8.0 LTS, including patch upgrades and the 7.0 → 8.0 upgrade.
- One image: `ghcr.io/sagh0900/zabbix-operator`.

## Quick start

Requires CloudNativePG.

```sh
VERSION=v0.1.0
kubectl apply -f https://github.com/sagh0900/zabbix-operator/releases/download/$VERSION/install.yaml
kubectl apply -f examples/database.yaml      # a CNPG cluster and its ZabbixDatabase
kubectl apply -f examples/system.yaml        # Zabbix: two HA servers, frontend, web service
kubectl -n zabbix get zsys -w
```

## Documentation

| Guide | Contents |
|---|---|
| [Installation](docs/install.md) | Requirements, operator, database, first system, monitoring, uninstall |
| [Operations](docs/operations.md) | State, configuration, scaling, failover, maintenance, suspend, proxies, troubleshooting |
| [Upgrades](docs/upgrades.md) | Zabbix patch and 7.0 → 8.0 upgrades, PostgreSQL upgrades, operator upgrades |
| [Migration](docs/migration.md) | Moving a running plain-manifest or Helm installation to the operator |
| [Alerts](docs/alerts.md) | Runbook for every alert |
| [Architecture](docs/architecture.md) | The design, every field and behaviour |
| [Examples](examples) | Manifests, validated against the CRDs by the test suite |

[AGENTS.md](AGENTS.md) holds the contribution rules.

## Build

```sh
make test              # unit and envtest suites
make lint              # golangci-lint
make build             # bin/manager
make docker-build      # ghcr.io/sagh0900/zabbix-operator:v$(cat VERSION)
make build-installer   # dist/install.yaml
```

## License

Apache License 2.0. See [LICENSE](LICENSE).
