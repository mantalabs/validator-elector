# validator-elector

validator-elector helps run a highly available [Celo
validator](https://docs.celo.org/getting-started/mainnet/running-a-validator-in-mainnet)
on Kubernetes.

## Usage

Add a validator-elector container to run as a sidecar to each `Pod`
running a Celo validator container. The validator-elector containers
race to acquire a lock. The validator-elector container that
successfully acquires the lock makes an JSONRPC request to the Celo
validator to start validation.

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
* [Simple Leader Election with Kubernetes and Docker (code)](https://github.com/kubernetes-retired/contrib/tree/master/election)
* [Simple leader election with Kubernetes and Docker (blog)](https://kubernetes.io/blog/2016/01/simple-leader-election-with-kubernetes/)
* [Leader Election inside Kubernetes](https://carlosbecker.com/posts/k8s-leader-election)
