# End-to-end environment

A real Kubernetes cluster, a real Prometheus, real pods holding real extended
resources — and a fake dcgm-exporter whose readings we control exactly.

This exists because it earns its keep. Five bugs reached this directory that the
unit suite and the demo fixture both missed, including one that reported a GPU
running at 78% utilization as having done no work at all. Everything here is
cheap to stand up and worth standing up before a release.

## What is fake, and why only that

| Piece | Real? | Why |
|---|---|---|
| Cluster, kubelet, scheduler | real | node labels, taints, pool naming and the autoscaler config are exactly the things ullage reads and gets wrong |
| Prometheus | real | staleness, chunking, point limits and label rewriting are where the bugs live |
| Pods, extended resources | real | ullage reads `limits`/`requests` off live pods |
| dcgm-exporter | **fake** | GPU nodes cost real money, and a fake exporter is the only way to *dictate* that one card is at 78% and another at 0% |

The exporter serves DCGM-shaped metrics on `:9400` from a `SPEC` env var of
`gpu:util:power:pod:namespace` tuples, so a scenario is one string.

## Stand it up

```bash
az group create -n ullage-e2e -l westus2
az aks create -g ullage-e2e -n ullage-e2e --node-count 2 \
  --node-vm-size Standard_D4s_v5 --enable-cluster-autoscaler --min-count 2 --max-count 4
az aks get-credentials -g ullage-e2e -n ullage-e2e

kubectl create ns ullage-e2e; kubectl create ns ml
kubectl apply -f e2e/exporter.yaml -f e2e/prom.yaml

# Advertise GPUs the nodes do not have. Kubelet propagates capacity to
# allocatable within about 20 seconds.
for n in $(kubectl get nodes -o name); do
  kubectl patch "$n" --subresource=status --type=json \
    -p='[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"2"}]'
done

kubectl apply -f e2e/workloads.yaml
kubectl -n ullage-e2e port-forward svc/prometheus 9090:9090 &
```

## What a correct run looks like

```bash
ullage --prometheus http://127.0.0.1:9090 --window 10m --idle-threshold 2m --step 1m
```

- `ml/idle-notebook-0` is reported, holding **1** GPU
- `ml/llama-train-0` is **absent** — it is at 78% utilization
- the second node pool appears under *Unused by design*, because the cluster
  autoscaler really is holding a floor of 2

Run it again with `--window 14d`. The verdict must not change. Two different
window lengths disagreeing about the same cluster is how the aggregate-vs-shape
bug was found.

## Tear down

```bash
az group delete -n ullage-e2e --yes --no-wait
```
