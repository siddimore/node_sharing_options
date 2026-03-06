# Gossip Sharding Demo

A distributed systems demonstration comparing four load balancing strategies: **Gossip Protocol**, **Redis Central Registry**, **Zookeeper Coordination**, and **Consistent Hashing**.

## Overview

This project implements a 6-node cluster where each node can route requests based on different load balancing strategies. It demonstrates the trade-offs between decentralized (gossip), centralized (Redis/Zookeeper), and deterministic (hash) approaches.

| Strategy | Load Aware | Fault Tolerant | Consistency | Complexity |
|----------|------------|----------------|-------------|------------|
| **Gossip** | Real-time | No SPOF | Eventual | High |
| **Redis** | Real-time | SPOF | Strong | Medium |
| **Zookeeper** | Real-time | Quorum-based | Strong (CP) | Medium-High |
| **Hash** | Static | Rehash needed | Perfect | Low |

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                       Docker Network                         │
│                                                              │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐                      │
│  │Replica-1│  │Replica-2│  │Replica-3│                      │
│  │  :8081  │  │  :8082  │  │  :8083  │                      │
│  └────┬────┘  └────┬────┘  └────┬────┘                      │
│       │            │            │                            │
│       └────────────┼────────────┘                            │
│                    │                                         │
│  ┌─────────┐  ┌────┴────┐  ┌───────────┐                  │
│  │Replica-4│  │  Redis  │  │ Zookeeper │                  │
│  │  :8084  │  │  :6379  │  │   :2181   │                  │
│  └─────────┘  └─────────┘  └───────────┘                  │
│       │            │                                         │
│  ┌────┴────┐  ┌────┴────┐                                  │
│  │Replica-5│  │Replica-6│                                  │
│  │  :8085  │  │  :8086  │                                  │
│  └─────────┘  └─────────┘                                  │
└─────────────────────────────────────────────────────────────┘
```

## Prerequisites

- Docker and Docker Compose
- Go 1.21+ (for CLI tools)

## Quick Start

```bash
# Start cluster in gossip mode (default)
MODE=gossip docker-compose up --build -d

# Check cluster status
go run cmd/cli/main.go status -all

# Send a test request
go run cmd/cli/main.go request -key "user-123" -label "payments"

# Run load test
go run cmd/cli/main.go loadtest -duration 15 -concurrency 30

# Stop cluster
docker-compose down
```

## Load Balancing Modes

### 1. Gossip Protocol (`MODE=gossip`)

Nodes exchange state with random peers using [HashiCorp's memberlist](https://github.com/hashicorp/memberlist). Load information spreads epidemically through the cluster.

```bash
MODE=gossip docker-compose up --build -d
```

**Pros:**
- No single point of failure
- Low routing latency (~0ms decisions)
- Automatic failure detection and recovery
- Network partition tolerant

**Cons:**
- Eventual consistency (200-500ms stale data)
- Higher implementation complexity
- Constant background network traffic (~1-2 KB/s per node)

### 2. Redis Central Registry (`MODE=redis`)

All nodes publish their load to Redis. Routing decisions query Redis for current cluster state.

```bash
MODE=redis docker-compose up --build -d
```

**Pros:**
- Strong consistency
- Simple implementation
- Immediate updates

**Cons:**
- Single point of failure (Redis)
- +1-5ms latency per routing decision
- Redis becomes bottleneck at scale

### 3. Zookeeper Coordination (`MODE=zookeeper`)

Nodes register as ephemeral znodes with load metadata. Watches provide instant notifications when cluster changes.

```bash
MODE=zookeeper docker-compose up --build -d
```

**Pros:**
- Strong consistency via ZAB protocol
- Automatic failure detection (ephemeral nodes)
- Push-based updates via watches
- Battle-tested (Kafka, HBase, Hadoop)

**Cons:**
- Write bottleneck (all writes go through leader)
- +2-10ms latency for consensus
- Operational complexity (requires quorum)

### 4. Consistent Hashing (`MODE=hash`)

Deterministic routing based on hash function. No inter-node communication.

```bash
MODE=hash docker-compose up --build -d
```

**Pros:**
- Zero coordination overhead
- Perfect cache locality
- Simple and fast

**Cons:**
- No load awareness (hot keys create hot nodes)
- No failure detection
- Uneven distribution without virtual nodes

## CLI Commands

```bash
# Single request
go run cmd/cli/main.go request -key "user-123" -label "auth"

# Benchmark with concurrency
go run cmd/cli/main.go benchmark -requests 100 -concurrency 10

# Label-based benchmark (tests cache affinity)
go run cmd/cli/main.go label-benchmark -duration 15 -concurrency 30

# Continuous load test
go run cmd/cli/main.go loadtest -duration 30 -concurrency 50

# Chaos test (kill nodes during load)
go run cmd/cli/main.go chaos -duration 30 -concurrency 30 -kill 2

# View gossip metrics
go run cmd/cli/main.go metrics

# Check status of all replicas
go run cmd/cli/main.go status -all

# Compare all four modes
go run cmd/cli/main.go compare
```

## Benchmark Results

### Normal Operation (15 seconds, 30 concurrent workers)

| Metric | Gossip | Redis | Zookeeper | Hash |
|--------|--------|-------|-----------|------|
| **Throughput** | 213 req/s | 220 req/s | 312 req/s | 84 req/s |
| **Avg Latency** | 140ms | 136ms | 95ms | 357ms |
| **P95 Latency** | 378ms | 306ms | 142ms | 952ms |
| **P99 Latency** | 686ms | 568ms | 591ms | 1002ms |

### Chaos Test (30s, 30 workers, 2 nodes killed)

| Metric | Gossip | Redis | Zookeeper | Hash |
|--------|--------|-------|-----------|------|
| **Success Rate** | 74.40% | 75.23% | 77.28% | 77.28% |
| **Throughput** | 183 req/s | 205 req/s | 179 req/s | 92 req/s |
| **P99 Latency** | 693ms | 439ms | 780ms | 679ms |
| **Max Latency** | 5,045ms | 5,157ms | 1,315ms | 1,176ms |

See [COMPARISON.md](COMPARISON.md) for detailed analysis.

## Project Structure

```
├── main.go              # Replica server implementation
├── gossip/
│   └── manager.go       # Gossip protocol manager (memberlist)
├── zookeeper/
│   └── manager.go       # Zookeeper coordination manager
├── routing/
│   └── router.go        # Consistent hash router
├── cmd/cli/
│   └── main.go          # CLI tool for testing
├── docker-compose.yml   # 6-node cluster definition
├── Dockerfile           # Go build container
└── COMPARISON.md        # Detailed strategy comparison
```

## Configuration

Environment variables for each replica:

| Variable | Description | Default |
|----------|-------------|---------|
| `MODE` | Load balancing mode: `gossip`, `redis`, `zookeeper`, `hash` | `gossip` |
| `REPLICA_ID` | Unique identifier for this replica | - |
| `PORT` | HTTP server port | `8080` |
| `ALL_REPLICAS` | Comma-separated list of all replicas | - |
| `REDIS_ADDR` | Redis server address | `redis:6379` |
| `ZK_ADDR` | Zookeeper server address | `zookeeper:2181` |

## API Endpoints

Each replica exposes:

| Endpoint | Description |
|----------|-------------|
| `POST /request` | Process a request with routing |
| `GET /status` | Return node status and load |
| `GET /gossip-metrics` | Gossip protocol statistics (gossip mode) |
| `POST /slowdown` | Simulate node slowdown for testing |

## How It Works

### Request Flow

1. Client sends request to any replica
2. Replica determines target node based on mode:
   - **Gossip**: Check local peer state, route to lowest load
   - **Redis**: Query Redis for cluster loads, route to lowest
   - **Zookeeper**: Read peer znodes (cached via watches), route to lowest load
   - **Hash**: Compute hash of key, deterministically select node
3. If target is self, handle locally; otherwise forward
4. Response includes routing metadata for analysis

### Label Caching

Requests can include a `label` (e.g., "payments", "auth"). Nodes cache labels they've processed, simulating warm state. Subsequent requests for the same label route to cached nodes for faster processing.

## When to Use Each Strategy

| Scenario | Recommended |
|----------|-------------|
| Multi-region deployment | Gossip |
| High availability required | Gossip |
| Already using Redis | Redis |
| Single region, simple setup | Redis |
| Need strong consistency + auto failure detection | Zookeeper |
| Already using Kafka/ZK infrastructure | Zookeeper |
| Uniform load distribution | Hash |
| Cache layer (memcached-style) | Hash |

## License

MIT
