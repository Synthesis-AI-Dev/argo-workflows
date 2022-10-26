# Proposal for Dynamic Semaphores

## Introduction

The motivation for this feature is to allow Argo to distribute locks using various strategies. We have heard two specific strategy requests so far:

1. Argo distributes keys in equal number based on Workflows "rebalance" keys.

2. Argo uses rebalance keys, but each rebalance key can have a different weight. This could represent tiers, where different tiers have access to different proportions of resources.

See "Appendix" for a full list of meeting notes and discussions on this feature.

## Spec proposal (first iteration)

Our proposal is broken into two pieces:

* Configuration: How can this feature be used?
* Implementation: What Argo code needs to change to achieve this?

## Configuration

The proposed configuration introduces only a single new property, called the `rebalanceKey`. This still respects the limit set by configMapRef, but ensures that all Workflows holding the same `rebalanceKey` receive the same access to resources as all other `rebalanceKeys`.

If no rebalance key is set, original behavior is retained.

If _some_ Workflows have `rebalanceKey` set, but others don't, we consider all Workflows without the key to have their own unique key (i.e. they are grouped together).

```
spec:
	synchronization:
		semaphore:
			configMapRef:
				name: "my-config"
				key: "template"
			rebalanceKey: ""
```

### Example

Suppose we have limit = 10 defined in ConfigMap for semahore named S, and a workflow W with 100 sibling templates, all which require S. We first submit one instance of W (let's call it W1) with `rebalanceKey` set to "key-A", and observe that 10 Pods run, because the limit is set to 10. We then submit a second Workflow (called W2) with rebalanceKey set to "key-B". As W1's pods finish, _only_ W2's pods will spin up, until we reach a point where 5 of W1's pods and 5 of W2's pods are running, maintaining this balance until one instance of W no longer has any pods to execute.

## Implementation

Following our meeting on Tuesday, Aug 23rd (see Appendix), we decided to assign rebalance keys via the Workflow itself.

An assumption that we stressed in this meeting is that we do not claim to respect priority if this new "rebalance" strategy is used.

We can view both priority and rebalance as two "strategies" in theory. My implementation, at the moment enhances `PrioritySemaphore`, instead of creating a new type of Semaphore. But it might be worth discussing the possibility of creating a new `RebalanceSemaphore` instead. This comes with additional complexity, as well as more spec changes (we'd have to update `SemaphoreStatus`, for example)


### Meeting notes and discussions

Existing discussions / proposals:
* [Dynamic semaphore implementation discussion](https://github.com/argoproj/argo-workflows/discussions/9382)
* [Original issue, which was referred to as "resource sharing"](https://github.com/argoproj/argo-workflows/issues/8982)
* Maintainers meetings, particularly the one on 19th July, 2022
* [Original slide deck, outlining a POC that kicked off these discussions](https://docs.google.com/presentation/d/1izeZeHZxz54P3njVRAJVVczMpDmGtkU7hD_CUtbaqgY/edit#slide=id.p)

After these, and a few other ad hoc discussions, maintainers decided to meet to decide how we can implement a feature that provides "resource sharing" functionality within Argo's existing feature set.

On Tuesday, Aug 23rd, Bala, Julie V, and Robert K met to discuss implementation. This meeting can be seen [here](https://us06web.zoom.us/rec/share/nBroocZX1E2Ut5XM9Cgdcs3v4qDE911ToDY_P3Hh4kylRC14ozAEoFkBhKevQOo.duY7m_dgb6CVHRNK).

As a result of that call, Robert was tasked with writing a more formal spec proposal and submitting it for review prior to the September 6 maintainers meeting.