# Working on this repository

Instructions for anyone, human or automated, changing this code.

## Read first
- [docs/architecture.md](docs/architecture.md) is the design. Code that disagrees with it is
  a bug, or the document needs changing in the same pull request.
- [docs/roadmap.md](docs/roadmap.md) lists the planned pull requests while it exists.

## Rules
- Two CRDs only: `ZabbixDatabase` and `ZabbixSuite`. Do not add CRDs.
- The operator never writes to CNPG objects and never creates database roles.
- Server, web, web service and proxies are bare Pods owned by the suite; agents are a
  DaemonSet. Do not introduce Deployments or StatefulSets.
- Supported Zabbix lines are 7.0 and 8.0. Major upgrades require explicit approval.
- No external leader election for Zabbix server: native HA plus the readiness probe on
  10051 decides routing.
- Never stop Zabbix because the database is unavailable, and never write to CNPG.
- Write code and documentation in the present tense about what the system does. Do not
  reference past versions, earlier designs, bug numbers or dates.
- Keep it lean: no speculative options, no unused fields, no dead code.

## Build and test
- `make test` runs unit and envtest suites.
- After changing API types, regenerate CRDs and deepcopy code and commit the output.
- Go files start with `hack/boilerplate.go.txt`.
- Every behaviour change ships with tests.

## Versions and images
- `VERSION` holds the semantic version. Bump MAJOR for breaking API changes, MINOR for new
  behaviour, PATCH for fixes. Never reuse an image tag.
- One image, `ghcr.io/sagh0900/zabbix-operator`; Jobs run it with a subcommand.
- Releases are cut by pushing a `v*` tag; CI builds and publishes the image and release.

## Git
- Work on a branch and open a pull request; `main` is protected.
- Conventional commit subjects (`feat:`, `fix:`, `docs:`, `test:`, `chore:`, `ci:`).
- No tool or assistant attribution in commits or pull requests.
