# Load Balancing Strategies: Gossip vs Redis vs Consistent Hashing

This document compares three load balancing strategies for distributed systems, demonstrating their trade-offs in terms of performance, fault tolerance, and implementation complexity.

## Overview

| Strategy | Load Aware | Fault Tolerant | Consistency | Complexity |
|----------|------------|----------------|-------------|------------|
| **Gossip** | Real-time | No SPOF | Eventual | High |
| **Redis** |  Real-time | SPOF | Strong | Medium |
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

## Benchmark Results

### Normal Operation (15 seconds, 30 concurrent workers)

| Metric | Gossip | Redis | Hash |
|--------|--------|-------|------|
| **Throughput** | 213 req/s | 220 req/s | 84 req/s |
| **Avg Latency** | 140ms | 136ms | 357ms |
| **P50 Latency** | 107ms | 110ms | 305ms |
| **P95 Latency** | 378ms | 306ms | 952ms |
| **P99 Latency** | 686ms | 568ms | 1002ms |
| **Cache Hit** | 97.8% | 98.3% | 96.4% |

### Key Findings

1. **Gossip & Redis perform similarly** in single-region setups with low Redis latency
2. **Hash mode is 2.5x slower** due to hot spots (5 labels → some hash to same node)
3. **Tail latency (P99) is critical**: Hash mode shows 1 second P99 vs ~600ms for adaptive modes

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

# Hash mode
MODE=hash docker-compose up --build -d && sleep 5
go run cmd/cli/main.go chaos -duration 30 -concurrency 30 -kill 2
```

### Actual Results (20s test, 20 workers, 2 nodes killed)

| Metric | Gossip | Redis | Hash |
|--------|--------|-------|------|
| **Total Requests** | 4,580 | 4,087 | 4,690 |
| **Success Rate** | 74.87% | 74.28% | 70.49% |
| **Throughput** | 171 req/s | 152 req/s | 165 req/s |
| **Failed Before Kill** | 18 | 11 | 13 |
| **Failed After Kill** | 1,133 | 1,040 | 1,371 |
| **Success After Kill** | 2,331 | 2,091 | 2,653 |
| **P99 Latency** | 377ms | 596ms | 277ms |
| **Max Latency** | 736ms | 5,113ms | 448ms |

### Key Observations

1. **All modes show ~25-30% failure rate** because test client sends to all ports (simulating external LB)
2. **Redis shows 5-second max latency** - HTTP timeout waiting for dead node response
3. **Gossip has lowest P99** during chaos - peers detected dead nodes faster
4. **The real difference appears in production** with single entry point:
   - **Gossip**: Detects failures via probe (~500ms), stops routing
   - **Redis**: Dead node's TTL expires (1s), disappears from queries
   - **Hash**: No detection until client timeout

### Expected Behavior

| Mode | During Failure | Recovery |
|------|----------------|----------|
| **Gossip** | Automatic rerouting within 500ms-1s | New nodes discovered via gossip |
| **Redis** | Still works if Redis up | Immediate via Redis |
| **Hash** | Requests to dead nodes fail | Requires rehash/restart |

### Failure Impact

- **Gossip**: Other nodes detect failure via probe timeout, stop routing to dead node
- **Redis**: Works as long as Redis is up; Redis down = total failure
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
| Hash | ~50 | none | Low |

---

## Running the Demo

### Prerequisites
- Docker and Docker Compose

### Commands

```bash
# Start in gossip mode
MODE=gossip docker-compose up --build -d

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
```

---

## Conclusion

**For most production systems requiring load-aware routing:**
- Start with **Redis** for simplicity if you already have it
- Graduate to **Gossip** when you need multi-region or higher availability
- Use **Consistent Hashing** only when load is uniform and you need cache locality

The gossip protocol's complexity is justified when:
1. Redis becomes a bottleneck or SPOF concern
2. Multi-region routing is required
3. Network partition tolerance is critical
