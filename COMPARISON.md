# Load Balancing Strategies: Gossip vs Redis vs Zookeeper vs Consistent Hashing

This document compares four load balancing strategies for distributed systems, demonstrating their trade-offs in terms of performance, fault tolerance, and implementation complexity.

## Overview

| Strategy | Load Aware | Fault Tolerant | Consistency | Complexity |
|----------|------------|----------------|-------------|------------|
| **Gossip** | Real-time | No SPOF | Eventual | High |
| **Redis** |  Real-time | SPOF | Strong | Medium |
| **Zookeeper** | Real-time | Quorum-based | Strong (CP) | Medium-High |
| **Hash** |  Static | Rehash needed | Perfect | Low |

---

## 1. Gossip Protocol (Decentralized)

### How It Works
- Nodes periodically exchange state with random peers
- Information spreads epidemically through the cluster
- Each node maintains a local view of cluster state

### Architecture
```
┌─────┐    gossip    ┌─────┐    gossip    ┌─────┐
│Node1│◄────────────►│Node2│◄────────────►│Node3│
└─────┘              └─────┘              └─────┘
    ▲                    ▲                    ▲
    │                    │                    │
    └────────────────────┴────────────────────┘
                     peer-to-peer
```

### Pros
- **No Single Point of Failure**: Any node can fail without breaking load balancing
- **Low Routing Latency**: Decisions made locally (~0ms)
- **Automatic Recovery**: Nodes detect failures and route around them
- **Scalable**: Bandwidth grows linearly O(N), not quadratically
- **Network Partition Tolerant**: Continues working with partial connectivity

### Cons
- **Eventual Consistency**: Load info may be 200-500ms stale
- **Implementation Complexity**: Requires understanding of gossip protocols
- **Convergence Time**: New nodes take time to be discovered
- **Network Overhead**: Constant background traffic (~1-2 KB/s per node)

### Configuration
```go
GossipInterval:    200ms   // Peer-to-peer state exchange
ProbeInterval:     500ms   // Health checks
PushPullInterval:  1000ms  // Full state synchronization
BroadcastInterval: 300ms   // Load updates
```

### When to Use
- Multi-region deployments
- Systems requiring high availability
- When Redis/centralized store is not available
- Latency-sensitive routing decisions

---

## 2. Redis Central Registry

### How It Works
- All nodes publish their load to Redis
- Before routing, query Redis for current cluster state
- Single source of truth for load information

### Architecture
```
┌─────┐              ┌─────────┐              ┌─────┐
│Node1│─────────────►│  Redis  │◄─────────────│Node2│
└─────┘              │(Central)│              └─────┘
                     └─────────┘
                          ▲
                          │
                     ┌─────┐
                     │Node3│
                     └─────┘
```

### Pros
- **Strong Consistency**: All nodes see same state
- **Simple Implementation**: Just GET/SET operations
- **Immediate Updates**: No convergence delay
- **Rich Queries**: Can do complex queries on cluster state

### Cons
- **Single Point of Failure**: Redis down = blind routing
- **Network Latency**: +1-5ms per routing decision (Redis RTT)
- **Scalability Limit**: Redis becomes bottleneck at scale
- **Operational Overhead**: Another service to maintain

### Configuration
```go
UpdateInterval: 200ms       // Publish load to Redis
TTL:            1 second    // Auto-expire stale entries
```

### When to Use
- Already have Redis in infrastructure
- Need strong consistency for routing
- Smaller clusters (<100 nodes)
- Can tolerate Redis as SPOF (with Redis Cluster)

---

## 3. Consistent Hashing

### How It Works
- Hash function maps requests to nodes deterministically
- Same key always goes to same node
- No inter-node communication needed

### Architecture
```
        Request (key="user-123")
                │
                ▼
        ┌───────────────┐
        │  Hash(key)    │ ──► Node2
        └───────────────┘
        
        Hash Ring:
        ┌─────────────────────────────┐
        │    Node1    Node2    Node3  │
        │     │         │        │    │
        │ ────┼─────────┼────────┼──► │
        │     0        33%      66%   │
        └─────────────────────────────┘
```

### Pros
- **Zero Coordination**: No network calls for routing
- **Perfect Cache Locality**: Same key = same node
- **Simple**: Easy to implement and understand
- **Minimal Rehash**: Only 1/N keys move when node added/removed

### Cons
- **No Load Awareness**: Hot keys create hot nodes
- **No Failure Detection**: Routes to dead nodes until timeout
- **Uneven Distribution**: Without virtual nodes, can be imbalanced
- **Cache Stampede**: Node failure causes surge to neighbor

### When to Use
- Cache layers (memcached, Redis cluster)
- Partitioned databases
- When load is naturally uniform
- Stateless computation (any node can handle any request)

---

## 4. Zookeeper Coordination

### How It Works
- Each node registers as an ephemeral znode in Zookeeper
- Load metadata stored in znode data (updated every 200ms)
- Nodes watch the parent path for changes (instant notifications)
- Ephemeral nodes auto-delete when session ends (failure detection)

### Architecture
```
┌─────┐    watch     ┌───────────┐    watch     ┌─────┐
│Node1│─────────────►│ Zookeeper │◄─────────────│Node2│
└─────┘              │ (Quorum)  │              └─────┘
    │                └───────────┘                │
    │                     ▲                       │
    │                     │                       │
    └───── ephemeral ─────┼───── ephemeral ───────┘
           znodes         │        znodes
                     ┌─────┐
                     │Node3│
                     └─────┘
```

### Pros
- **Strong Consistency (CP)**: All nodes see same state via ZAB protocol
- **Automatic Failure Detection**: Ephemeral nodes deleted on session timeout
- **Watch Notifications**: Instant updates when cluster changes
- **Battle-tested**: Used by Kafka, HBase, Hadoop for coordination
- **No Polling Needed**: Watches push changes to clients

### Cons
- **Write Bottleneck**: All writes go through leader
- **Latency**: +2-10ms per write (consensus overhead)
- **Operational Complexity**: Requires odd number of nodes (3, 5, 7)
- **Session Management**: Must handle session expiry and reconnection
- **Not for High-Write Workloads**: ZK optimized for reads, not writes

### Configuration
```go
UpdateInterval:   200ms      // Publish load to ZK
SessionTimeout:   10s        // Session expiry (failure detection)
WatchPath:        /nodes     // Path to watch for changes
```

### When to Use
- Need strong consistency guarantees
- Already using Zookeeper in infrastructure (Kafka, etc.)
- Require automatic failure detection via ephemeral nodes
- Cluster coordination beyond just load balancing
- Can tolerate slightly higher write latency

---

## Benchmark Results

### Normal Operation (15 seconds, 30 concurrent workers)

| Metric | Gossip | Redis | Zookeeper | Hash |
|--------|--------|-------|-----------|------|
| **Throughput** | 213 req/s | 220 req/s | 312 req/s | 84 req/s |
| **Avg Latency** | 140ms | 136ms | 95ms | 357ms |
| **P50 Latency** | 107ms | 110ms | 83ms | 305ms |
| **P95 Latency** | 378ms | 306ms | 142ms | 952ms |
| **P99 Latency** | 686ms | 568ms | 591ms | 1002ms |
| **Cache Hit** | 97.8% | 98.3% | 100% | 96.4% |

### Key Findings

1. **Zookeeper achieved highest throughput** (312 req/s) due to efficient watch-based caching
2. **Zookeeper has lowest avg/P50/P95 latency** - local cache updated via watches, no per-request queries
3. **Hash mode is 3-4x slower** due to hot spots (5 labels → some hash to same node)
4. **Tail latency (P99) is critical**: Hash mode shows 1 second P99 vs ~600ms for adaptive modes

---

## Chaos Test Results (Node Failure)

Run chaos tests to see how each mode handles node failures:

```bash
# Gossip mode
MODE=gossip docker-compose up --build -d && sleep 5
go run cmd/cli/main.go chaos -duration 30 -concurrency 30 -kill 2

# Redis mode  
MODE=redis docker-compose up --build -d && sleep 5
go run cmd/cli/main.go chaos -duration 30 -concurrency 30 -kill 2

# Zookeeper mode
MODE=zookeeper docker-compose up --build -d && sleep 5
go run cmd/cli/main.go chaos -duration 30 -concurrency 30 -kill 2

# Hash mode
MODE=hash docker-compose up --build -d && sleep 5
go run cmd/cli/main.go chaos -duration 30 -concurrency 30 -kill 2
```

### Actual Results (30s test, 30 workers, 2 nodes killed)

| Metric | Gossip | Redis | Zookeeper | Hash |
|--------|--------|-------|-----------|------|
| **Total Requests** | 7,363 | 8,167 | 6,966 | 3,561 |
| **Success Rate** | 74.40% | 75.23% | 77.28% | 77.28% |
| **Throughput** | 183 req/s | 205 req/s | 179 req/s | 92 req/s |
| **Failed Before Kill** | 30 | 19 | 21 | 30 |
| **Failed After Kill** | 1,855 | 2,004 | 1,562 | 779 |
| **Success After Kill** | 3,534 | 3,755 | 3,139 | 1,646 |
| **P99 Latency** | 693ms | 439ms | 780ms | 679ms |
| **Max Latency** | 5,045ms | 5,157ms | 1,315ms | 1,176ms |

### Key Observations

1. **Zookeeper has best success rate** (77.28%) tied with Hash - ephemeral nodes auto-delete on failure
2. **Redis has highest throughput** during chaos (205 req/s) - simple GET/SET operations
3. **Gossip & Redis show 5-second max latency** - HTTP timeout waiting for dead node response
4. **Zookeeper max latency (1,315ms)** much better than Gossip/Redis - watch-based failure detection
5. **Hash mode is 2x slower throughput** due to hot spots, but lower max latency (no forwarding)
6. **The real difference appears in production** with single entry point:
   - **Gossip**: Detects failures via probe (~500ms), stops routing
   - **Redis**: Dead node's TTL expires (1s), disappears from queries
   - **Zookeeper**: Ephemeral znode deleted (~session timeout), watch notifies peers
   - **Hash**: No detection until client timeout

### Expected Behavior

| Mode | During Failure | Recovery |
|------|----------------|----------|
| **Gossip** | Automatic rerouting within 500ms-1s | New nodes discovered via gossip |
| **Redis** | Still works if Redis up | Immediate via Redis |
| **Zookeeper** | Automatic rerouting via watch | New nodes register ephemeral znodes |
| **Hash** | Requests to dead nodes fail | Requires rehash/restart |

### Failure Impact

- **Gossip**: Other nodes detect failure via probe timeout, stop routing to dead node
- **Redis**: Works as long as Redis is up; Redis down = total failure
- **Zookeeper**: Ephemeral znode deleted, watch triggers re-read; ZK down = fallback
- **Hash**: No detection; requests fail until client-side retry/timeout

---

## Network Overhead

### Gossip Protocol Traffic

Per node (6-node cluster):
- Messages: ~17-18/sec sent, ~11-14/sec received
- Bandwidth: ~1.2 KB/s sent, ~0.8 KB/s received
- **Total overhead: ~120 KB/min per node**

This is negligible compared to actual request traffic (typically MB/s).

### Redis Traffic

Per node:
- SET operation every 200ms (load update)
- GET operation per routing decision
- ~10-50 KB/s depending on request rate

### Zookeeper Traffic

Per node:
- SET operation every 200ms (znode data update)
- Heartbeats to maintain session (~every 2s)
- Watch notifications (push-based, minimal)
- ~5-20 KB/s depending on cluster changes
- **Advantage**: Watches are push-based, no polling needed

---

## Decision Matrix

### Choose Gossip When:
- Multi-region or multi-datacenter deployment
- Cannot tolerate a single point of failure
- Need to operate during network partitions
- Willing to accept eventual consistency
- High availability is critical

### Choose Redis When:
- Already using Redis in the stack
- Single region/datacenter
- Need strong consistency for routing
- Can use Redis Cluster for HA
- Simpler implementation is preferred

### Choose Zookeeper When:
- Need strong consistency with automatic failure detection
- Already using Zookeeper (Kafka, HBase, etc.)
- Want push-based notifications via watches
- Cluster coordination beyond load balancing needed
- Can tolerate slightly higher write latency

### Choose Consistent Hashing When:
- Load is naturally uniform across keys
- Need perfect cache locality
- Building a cache layer (memcached-style)
- No need for load-aware routing
- Simplicity is paramount

---

## Implementation Complexity

| Strategy | Lines of Code | Dependencies | Learning Curve |
|----------|---------------|--------------|----------------|
| Gossip | ~400 | memberlist | High |
| Redis | ~100 | go-redis | Low |
| Zookeeper | ~300 | go-zookeeper | Medium |
| Hash | ~50 | none | Low |

---

## Running the Demo

### Prerequisites
- Docker and Docker Compose

### Commands

```bash
# Start in gossip mode
MODE=gossip docker-compose up --build -d

# Start in zookeeper mode
MODE=zookeeper docker-compose up --build -d

# Run load test
go run cmd/cli/main.go loadtest -duration 30 -concurrency 50

# Run chaos test (kill 2 nodes during load)
go run cmd/cli/main.go chaos -duration 30 -kill 2

# View gossip metrics
go run cmd/cli/main.go metrics

# Check cluster status
go run cmd/cli/main.go status -all

# Compare with other modes
MODE=redis docker-compose up --build -d
MODE=hash docker-compose up --build -d
MODE=zookeeper docker-compose up --build -d
```

---

## Conclusion

**For most production systems requiring load-aware routing:**
- Start with **Redis** for simplicity if you already have it
- Use **Zookeeper** if you need strong consistency + automatic failure detection
- Graduate to **Gossip** when you need multi-region or higher availability
- Use **Consistent Hashing** only when load is uniform and you need cache locality

The gossip protocol's complexity is justified when:
1. Redis/Zookeeper becomes a bottleneck or SPOF concern
2. Multi-region routing is required
3. Network partition tolerance is critical

Zookeeper is ideal when:
1. You already have ZK in your infrastructure (Kafka, etc.)
2. You need strong consistency for routing decisions
3. Automatic failure detection via ephemeral nodes is valuable
4. You want push-based updates via watches
