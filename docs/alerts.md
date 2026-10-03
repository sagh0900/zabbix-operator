# Alerts

Runbook for the alerts in `config/monitoring/prometheusrule.yaml`. Each alert links here
through its `runbook_url` annotation.

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
