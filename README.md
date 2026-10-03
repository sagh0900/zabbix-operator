# Zabbix Operator

A Kubernetes operator that runs Zabbix (server in native HA, frontend, web service,
proxies, agents) on a PostgreSQL database managed by
[CloudNativePG](https://cloudnative-pg.io).

- Two custom resources: `ZabbixDatabase` and `ZabbixSuite`.
- Zabbix 7.0 LTS and 8.0 LTS, including patch upgrades and the 7.0 → 8.0 upgrade.
- One image: `ghcr.io/sagh0900/zabbix-operator`.

See [docs/architecture.md](docs/architecture.md) for the design and
[AGENTS.md](AGENTS.md) for contribution rules.

## Install

Requires CloudNativePG.

```sh
kubectl apply -f https://github.com/sagh0900/zabbix-operator/releases/download/vX.Y.Z/install.yaml
```

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
