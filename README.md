# validator-elector

validator-elector helps run a [Celo
validator](https://docs.celo.org/getting-started/mainnet/running-a-validator-in-mainnet)
on Kubernetes with automatic failover. You can use validator-elector
to increase availability or enable rolling deployments.

## Usage

Add a validator-elector container to run as a sidecar to each `Pod`
running a Celo validator container. If the the Celo validator's
current block is recent, the validator-elector sidecar will race to
acquire a lock. The validator-elector container that successfully
acquires the lock makes an JSONRPC request to the Celo validator to
start validation.

The validator-elector periodically renews the lock. If the
validator-elector fails to renew the lock (e.g., `Node` crash) within
a default time period, other validator-elector containers can acquire
the lock.

Before exit, a validator-elector will release the lock and
make a JSONRPC request to the Celo validator to stop validation.

[e2e](./e2e) has an example usage.

## Testing

```
e2e/run.py
```

## Related

* [leaderelection Documentation](https://pkg.go.dev/k8s.io/client-go/tools/leaderelection)
* [Leader Election Example](https://github.com/kubernetes/client-go/tree/master/examples/leader-election)
* [Lease v1](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.19/#lease-v1-coordination-k8s-io)
* [Hotswapping Validator Nodes](https://docs.celo.org/validator-guide/node-upgrades#hotswapping-validator-nodes)
* [celo-blockchain 1.2.0 Release Notes](https://github.com/celo-org/celo-blockchain/releases/tag/v1.2.0)
