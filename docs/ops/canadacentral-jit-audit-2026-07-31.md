# Canada Central management node JIT audit

Date: 2026-07-31

Cluster: `prod-canadacentral-mgmt-1`

Node: `aks-userswft1-41073048-vmss000005`

## Scope

The audit checked whether the node was responsible for the `set-kubelet-parameters-for-scale` rollout failures and repeated `kube-state-metrics` restarts.

## Commands and results

### Node health

`kubectl get node` and `kubectl describe node` showed:

- Node `Ready=True`
- `MemoryPressure=False`
- `DiskPressure=False`
- `PIDPressure=False`
- `ContainerRuntimeProblem=False`
- `KubeletProblem=False`
- No taints and the node was schedulable
- Current usage was approximately 11.7 GiB of 256 GiB memory

### Kubelet parameter DaemonSet

`set-kubelet-parameters-for-scale` showed `18 desired`, `18 current`, `18 up-to-date`, and `18 available`. Every pod was `1/1 Ready` with zero restarts.

The pod on the affected node was healthy, but its log reported that `/usr/local/bin/node-exporter-startup.sh` did not exist. The script nevertheless printed a success message after the failed `grep` and `sed` commands.

### Kube-state-metrics

Two `kube-state-metrics` pods on the affected node were in `CrashLoopBackOff`, each with approximately 777 restarts. Both had:

- `Last State: OOMKilled`
- Exit code `137`
- Memory request `32Mi`
- Memory limit `64Mi`

The previous logs showed successful startup, API-server communication, and metrics-server binding before the container was killed.

The same ReplicaSet revision, `7956654957`, also crashed in other hosted clusters, while older revision `757457d4c` pods were running. This showed the issue was not isolated to the Canada Central node.

### Other events

Recent events included readiness probe timeouts, connection refusals, container kills, catalog-operator backoffs, and missing-secret mount failures across multiple hosted clusters. These were not sufficient to attribute the failures to the affected node.

## Decision

Do not delete, drain, or replace `aks-userswft1-41073048-vmss000005` based on this evidence. The confirmed actionable issue is the undersized `kube-state-metrics` memory limit. The controller change raises the request to `64Mi` and the limit to `128Mi`.

## Follow-up

Monitor the new deployment for OOMKills and restart counts after rollout. If OOMKills continue, increase the limit to `256Mi` and investigate the per-HostedControlPlane replica configuration.
