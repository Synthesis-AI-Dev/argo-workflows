# Rebalanced Semaphore Strategy

![GA](assets/ga.svg)

## Introduction

In some situations where Argo is orchestrating a service with many end users, it may be the case that we want these users to have equal access to a particular resource. As number of concurrent users increases or decreases, we want Argo to know each users' constantly-changing lock quota.

One example could be distribution of a finite number of software licenses, where a Running node holds a single license for the duration of its execution.

This feature extends the behavior of Argo's [semaphore](https://github.com/argoproj/argo-workflows/blob/master/docs/synchronization.md) so that, in addition to specifying a rate limit for the number of locks available, the Argo user can specify a "rebalance key" directly in the Workflow file.

All Workflows specifying distinct rebalance key R will have access to ${ 1 / total # distinct rebalance keys} of this semaphore's rate limit.

_Argo users who do not use this feature are implicitly using the already-existing [default](https://github.com/argoproj/argo-workflows/blob/master/docs/synchronization.md) strategy._

### Example

Suppose we have the following:
* Semaphore S with rate limit 55
* Workflow A with 100 child nodes all vying for S with rebalance key J
* Workflow B with 100 child nodes all vying for S with rebalance key J
* Workflow C with 500 child nodes all vying for S with rebalance key K

Suppose A, B, and C are submitted, in that order. Then:
* There are two distinct rebalance keys J and K (that are either pending or held)
* Because A was submitted before B, and A and B both have rebalance key J, all of A's child nodes will receive a lock from S before any of B's.
* Because A was submitted before B and C, A _could_ briefly receive all of the locks because none of B or C's resources were added to the queue of pending lock holders.
* The list of resources holding a lock from S should quite quickly converge to a list where 27 of the holders are from A and 26 are from B (or vice versa). This is the steady state until A (or C) completes.

Suppose A and B complete, but C is still running. Then:
* C will have access to all 55 locks.

## User guide

### Configuration

Suppose we want to configure some semaphore named "template" to use the rebalanced strategy:

The property `semaphoreStrategies` must be added to your configmap containing template's rate limit:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
 name: my-config
data:
  workflow: "1"
  template: "2"
  semaphoreStrategies: |
    template: "rebalanced"

Note that semaphores not included in `semaphoreStrategies` will use the [default](https://github.com/argoproj/argo-workflows/blob/master/docs/synchronization.md) strategy. A configmap with no `semaphoreStrategies` key will configure all semaphores to use the default strategy.

To use the rebalanced semaphore, a user should (but is not required to) specify a rebalance key inside of the `synchronization.semaphore` property:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  generateName: rebalance-key-test-
spec:
  arguments:
    parameters:
      - name: rebalancekey
        value: "example-key-123"
  entrypoint: rebalance-key-test-entrypoint
  templates:
  - name: rebalance-key-test-entrypoint
    steps:
    - - name: generate
        template: gen-number-list    
    - - name: synchronization-acquire-lock
        template: acquire-lock
        withParam: "{{steps.generate.outputs.result}}"
  - name: gen-number-list
    script:
      image: python:alpine3.6
      command: [python]
      source: |
        import json
        import sys
        json.dump([i for i in range(0, 15)], sys.stdout)
  - name: acquire-lock
    synchronization:
      semaphore:
        configMapKeyRef:
          name: my-config
          key: template
        rebalanceKey: "{{workflow.parameters.rebalancekey}}"
    container:
      image: alpine:latest
      command: [sh, -c]
      args: ["echo acquired lock; sleep $((20 + RANDOM % 11));"]
```

After setting up your config file as described above, run this workflow two times, each time using a different rebalance key:

Submit a workflow with "user-000": `argo submit rebalance-key-test.yaml -p 'rebalancekey=user-000'`

Submit a workflow with "user-001": `argo submit rebalance-key-test.yaml -p 'rebalancekey=user-001'`

At first, the workflow from the first submission should use both available locks. After one pod completes, notice that lock allocation converges so that each workflow has constant access to 1 lock until one  workflow finishes entirely, at which point the remaining workflow has access to both locks.

### Assumptions

The following is a list of operational assumptions that a rebalanced semaphore user should understand before deciding to use this feature:

* A single workflow _can_ use many distinct semaphores using the rebalanced strategy.
* A single workflow _can not_ use one particular semaphore with two rebalance keys, even if they are used in two distinct locations within that workflow. (For Argo devs: This is because when the Semaphore peeks at item vying for the lock, if workflow is same, it will automatically try to acquire, bypassing the fact that resources themselves are different)
* A semaphore that is using a semaphore with the rebalanced strategy will ignore priority
* Workflow and Config file formats are backward compatible, meaning that existing workflows that do not want to use the rebalanced strategy need to make no changes.

### Workflow example

1. [Rebalanced Semaphore](https://github.com/argoproj/argo-workflows/blob/master/examples/rebalance-key.yaml)