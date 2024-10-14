# Distributed Scheduling Design Document: Spot Instance Optimization Strategy

## Problem Definition
The objective of this strategy is to ensure that workloads are optimally balanced between spot and on-demand instances to reduce costs while maintaining reliability.

## Goals
1. **Maximize Spot Instance Utilization**: Preferentially schedule workloads on spot instances to cut costs.
2. **Minimize Service Interruptions**: Ensure workload continuity by using on-demand instances as fallbacks in case of spot instance terminations.
3. **No Change to Existing K8s Scheduler**: Avoid replacing or modifying the current scheduler in the cluster.
4. **Minimal Component Addition**: Implement the strategy with the least amount of new components or services to avoid adding complexity.

## Requirements

### Node Labels
- On-demand nodes have the label `node.kubernetes.io/capacity: on-demand`.
- Spot nodes have the label `node.kubernetes.io/capacity: spot`.

### Workload Types
- **Deployments**: Stateless workloads, which can tolerate rescheduling more easily.
- **StatefulSets**: Stateful workloads, where maintaining instance availability is critical.

## Distributed Scheduling Design

### 1. Node Affinity and Tolerations
- Use node affinity to prefer scheduling workloads onto spot instances where possible.
- Introduce a secondary affinity for on-demand nodes as fallback, ensuring critical workloads remain running even if spot instances are terminated.

### 2. Pod Anti-affinity
- Apply pod anti-affinity to spread workloads across spot and on-demand nodes, reducing the risk of all replicas being terminated at once.

### 3. Distributed Replication Strategy
- **Single Replica Workloads**: Prioritize on-demand nodes to ensure availability, using spot instances as secondary options if no on-demand capacity is available.
- **Multiple Replica Workloads**: Distribute replicas across both spot and on-demand nodes to ensure at least one replica remains available in the event of spot termination.

### 4. Spot Instance Monitoring - TODO
- Monitor spot instance termination signals through external mechanisms (handled separately from scheduling control).
- Once a termination signal is received, existing components will gracefully drain the spot nodes and reschedule workloads onto available nodes.

### 5. Graceful Fallback to On-Demand Instances
- If all spot nodes are preempted or unavailable, on-demand nodes automatically serve as fallback under the affinity rules.
- On-demand instances can be configured with a higher cost tolerance to ensure critical services are uninterrupted.

## Technique
Using an admission webhook to modify the affinity of the pods, applying the scheduling strategy.

### Steps:
1. Write the webhook in Golang and build it as a container image.
2. Deploy it in a deployment and expose it via a service.
3. Configure the `MutatingWebhookConfiguration` to register the webhook.

## Scheduling Strategy

### Pre-conditions
1. Only two kinds of nodes: on-demand and spot.
2. All nodes are in the same zone (inter-AZ not considered).
3. User-defined affinities have a higher priority.
4. Use annotations to control whether a pod follows the webhook scheduling strategy. Only the namespace and the workload have the lable ds-admission-webhook/autoSchedule: "enabled" can trigger the webhook.

### Strategy

#### Deployment
- **Single Replica**: Prioritize scheduling to spot nodes. If unavailable, schedule to on-demand nodes.
- **Multiple Replicas**: Prefer scheduling to spot nodes. If unavailable, fallback to on-demand nodes.

#### StatefulSet
- **Single Replica**: Schedule to on-demand nodes directly to ensure stability.
- **Multiple Replicas**: 70% should be scheduled to on-demand nodes and 30% to spot nodes to reduce costs while retaining stability. This ratio can be customized via annotations, but the on-demand ratio cannot be 0.

### Anti-affinity Configuration
For multi-replica StatefulSets and Deployments, consider using pod anti-affinity to ensure that replicas do not all get scheduled on the same node type, reducing risk.

## Affinity Examples

### 1. Deployment (Single Replica & Multi-Replica)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: stateless-app
spec:
  replicas: 3  # Change to 1 for single replica
  template:
    spec:
      affinity:
        nodeAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 1
              preference:
                matchExpressions:
                  - key: "node.kubernetes.io/capacity"
                    operator: In
                    values:
                      - "spot"  # Prefer scheduling to spot nodes
          requiredDuringSchedulingIgnoredDuringExecution:
            - nodeSelectorTerms:
                - matchExpressions:
                    - key: "node.kubernetes.io/capacity"
                      operator: In
                      values:
                        - "spot"
                        - "on-demand"  # Fallback to on-demand nodes
```
### 2. StatefulSet - Single Replica
```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: stateful-app-single
spec:
  replicas: 1
  template:
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - nodeSelectorTerms:
                - matchExpressions:
                    - key: "node.kubernetes.io/capacity"
                      operator: In
                      values:
                        - "on-demand"  # Single replica to ensure stability
```
### 3. StatefulSet - Multiple Replicas (70:30 Distribution)
```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: stateful-app-multiple
spec:
  replicas: 10
  template:
    spec:
      affinity:
        nodeAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 7
              preference:
                matchExpressions:
                  - key: "node.kubernetes.io/capacity"
                    operator: In
                    values:
                      - "on-demand"  # 70% prioritized on on-demand nodes for stability
            - weight: 3
              preference:
                matchExpressions:
                  - key: "node.kubernetes.io/capacity"
                    operator: In
                    values:
                      - "spot"  # 30% prioritized on spot nodes to save costs
```
### 4. Anti-Affinity Configuration
```yaml
affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 1
        podAffinityTerm:
          labelSelector:
            matchLabels:
              app: stateful-app
          topologyKey: "kubernetes.io/hostname"  # Ensure different replicas are spread across nodes
```
## Future Consideration
1. **Persistent Volume Topology**: Based on the pod's PVC (Persistent Volume Claim), the system locates the storage class, identifies the available zones within that storage class, and selects one at random.

2. **Topology Spread**: Using Kubernetes topologySpreadConstraints, you can distribute pods across different zones to minimize the impact of potential outages by spreading them out.

3. **Pre-scheduling and Spot Instance Fallback**: This involves pre-scheduling mechanisms to predict potential spot instance interruptions and implementing fallback strategies before the unavailability occurs.

4. **Spot-Friendly Pod Identification**: Identifying pods that are optimized or suitable for running on spot instances, ensuring efficient use of such instances.

5. **Rolling Updates and Scheduling Consistency**: Ensuring consistency in the scheduling process during rolling updates, maintaining stable and reliable deployments.