package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gossip_sharded_demo/gossip"
	"gossip_sharded_demo/routing"

	"github.com/redis/go-redis/v9"
)

type Replica struct {
	ID             string
	Port           int
	GossipMgr      *gossip.Manager
	Router         *routing.Router
	RedisClient    *redis.Client
	RedisAddr      string
	Mode           string // "gossip", "hash", or "redis"
	AllReplicas    []string
	loadScore      int64 // Current concurrent requests
	totalHandled   int64 // Total requests handled
	slowdownFactor int64 // Simulated slowdown multiplier (1-5)

	// Label cache - labels this node has "warmed up" for faster processing
	labelCacheMu sync.RWMutex
	labelCache   map[string]bool
}

type RequestResponse struct {
	Success       bool             `json:"success"`
	HandledBy     string           `json:"handled_by"`
	ReceivedBy    string           `json:"received_by"`
	Key           string           `json:"key"`
	Label         string           `json:"label,omitempty"`
	CacheHit      bool             `json:"cache_hit"`
	Forwarded     bool             `json:"forwarded"`
	LatencyMs     int64            `json:"latency_ms"`
	ProcessingMs  int64            `json:"processing_ms"`
	LoadAtHandle  int64            `json:"load_at_handle"`
	Mode          string           `json:"mode"`
	ClusterLoads  map[string]int64 `json:"cluster_loads,omitempty"`
	RoutingReason string           `json:"routing_reason,omitempty"`
}

type StatusResponse struct {
	ID             string           `json:"id"`
	Port           int              `json:"port"`
	LoadScore      int64            `json:"load_score"`
	TotalHandled   int64            `json:"total_handled"`
	Mode           string           `json:"mode"`
	ClusterSize    int              `json:"cluster_size"`
	SlowdownFactor int64            `json:"slowdown_factor,omitempty"`
	CachedLabels   []string         `json:"cached_labels,omitempty"`
	Peers          map[string]int64 `json:"peers,omitempty"`
}

func NewReplica(id string, port int, mode string, allReplicas []string, redisAddr string) *Replica {
	return &Replica{
		ID:             id,
		Port:           port,
		Mode:           mode,
		AllReplicas:    allReplicas,
		RedisAddr:      redisAddr,
		slowdownFactor: 1,
		labelCache:     make(map[string]bool),
	}
}

func (r *Replica) Start() error {
	// Initialize router (needed for both modes)
	r.Router = routing.NewRouter(r.AllReplicas)

	// Initialize based on mode
	if r.Mode == "gossip" {
		var err error
		r.GossipMgr, err = gossip.NewManager(r.ID, r.Port+1000)
		if err != nil {
			return fmt.Errorf("failed to start gossip: %v", err)
		}

		// Join cluster
		go r.joinCluster()

		// Broadcast load periodically
		go r.broadcastLoad()
	} else if r.Mode == "redis" {
		// Initialize Redis client
		r.RedisClient = redis.NewClient(&redis.Options{
			Addr:     r.RedisAddr,
			Password: "",
			DB:       0,
		})

		// Test connection
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.RedisClient.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("failed to connect to Redis: %v", err)
		}
		log.Printf("[%s] Connected to Redis at %s", r.ID, r.RedisAddr)

		// Publish load to Redis periodically
		go r.publishLoadToRedis()
	}

	// Simulate variable node load (some nodes become slow randomly)
	go r.simulateLoadChanges()

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/request", r.handleRequest)
	mux.HandleFunc("/forward", r.handleForward)
	mux.HandleFunc("/status", r.handleStatus)
	mux.HandleFunc("/health", r.handleHealth)
	mux.HandleFunc("/metrics", r.handleMetrics)

	addr := fmt.Sprintf(":%d", r.Port)
	log.Printf("[%s] Starting replica on port %d (mode: %s)", r.ID, r.Port, r.Mode)
	return http.ListenAndServe(addr, mux)
}

func (r *Replica) joinCluster() {
	time.Sleep(2 * time.Second) // Wait for other nodes to start

	for _, replica := range r.AllReplicas {
		if replica == r.ID {
			continue
		}
		gossipAddr := fmt.Sprintf("%s:%d", replica, r.Port+1000)
		if err := r.GossipMgr.Join([]string{gossipAddr}); err != nil {
			log.Printf("[%s] Failed to join %s: %v", r.ID, gossipAddr, err)
		}
	}
}

func (r *Replica) broadcastLoad() {
	ticker := time.NewTicker(200 * time.Millisecond)
	for range ticker.C {
		if r.GossipMgr != nil {
			r.GossipMgr.UpdateLoad(atomic.LoadInt64(&r.loadScore))
			// Also broadcast cached labels
			r.GossipMgr.UpdateCachedLabels(r.getCachedLabels())
		}
	}
}

// publishLoadToRedis publishes this node's load to Redis central registry
func (r *Replica) publishLoadToRedis() {
	ticker := time.NewTicker(200 * time.Millisecond)
	for range ticker.C {
		if r.RedisClient != nil {
			ctx := context.Background()
			load := atomic.LoadInt64(&r.loadScore)
			key := fmt.Sprintf("load:%s", r.ID)
			// Set with 1 second TTL so stale entries expire
			r.RedisClient.Set(ctx, key, load, 1*time.Second)

			// Also store cached labels
			labels := r.getCachedLabels()
			if len(labels) > 0 {
				labelKey := fmt.Sprintf("labels:%s", r.ID)
				r.RedisClient.Set(ctx, labelKey, strings.Join(labels, ","), 1*time.Second)
			}
		}
	}
}

// getLoadsFromRedis fetches all node loads from Redis
func (r *Replica) getLoadsFromRedis() (map[string]int64, error) {
	ctx := context.Background()
	result := make(map[string]int64)

	for _, replica := range r.AllReplicas {
		key := fmt.Sprintf("load:%s", replica)
		val, err := r.RedisClient.Get(ctx, key).Int64()
		if err == nil {
			result[replica] = val
		} else if err != redis.Nil {
			// Connection error
			return nil, err
		}
	}

	return result, nil
}

// getNodesWithLabelFromRedis fetches nodes that have a specific label cached
func (r *Replica) getNodesWithLabelFromRedis(label string) map[string]int64 {
	ctx := context.Background()
	result := make(map[string]int64)

	for _, replica := range r.AllReplicas {
		labelKey := fmt.Sprintf("labels:%s", replica)
		labelsStr, err := r.RedisClient.Get(ctx, labelKey).Result()
		if err != nil {
			continue
		}

		labels := strings.Split(labelsStr, ",")
		for _, l := range labels {
			if l == label {
				loadKey := fmt.Sprintf("load:%s", replica)
				load, err := r.RedisClient.Get(ctx, loadKey).Int64()
				if err == nil {
					result[replica] = load
				}
				break
			}
		}
	}

	return result
}

// getCachedLabels returns a copy of the cached labels
func (r *Replica) getCachedLabels() []string {
	r.labelCacheMu.RLock()
	defer r.labelCacheMu.RUnlock()
	labels := make([]string, 0, len(r.labelCache))
	for label := range r.labelCache {
		labels = append(labels, label)
	}
	return labels
}

// hasLabelCached checks if a label is in the cache
func (r *Replica) hasLabelCached(label string) bool {
	if label == "" {
		return true // No label means no cache needed
	}
	r.labelCacheMu.RLock()
	defer r.labelCacheMu.RUnlock()
	return r.labelCache[label]
}

// addToLabelCache adds a label to the cache
func (r *Replica) addToLabelCache(label string) {
	if label == "" {
		return
	}
	r.labelCacheMu.Lock()
	defer r.labelCacheMu.Unlock()
	if !r.labelCache[label] {
		r.labelCache[label] = true
		log.Printf("[%s] CACHE: Warmed up label '%s'", r.ID, label)
	}
}

// simulateLoadChanges randomly changes this node's slowdown factor
// to simulate real-world scenarios where nodes become overloaded
func (r *Replica) simulateLoadChanges() {
	// Disable random slowdown - label caching is the main differentiator now
	// Each node has different change frequency (5-10 seconds)
	interval := time.Duration(5000+rand.Intn(5000)) * time.Millisecond
	ticker := time.NewTicker(interval)

	for range ticker.C {
		// 20% chance to become slow (factor 2-3x), 80% chance normal
		var newFactor int64
		if rand.Float32() < 0.2 {
			newFactor = int64(2 + rand.Intn(2)) // 2 or 3
			log.Printf("[%s] SLOWDOWN: Processing %dx slower", r.ID, newFactor)
		} else {
			newFactor = 1
			oldFactor := atomic.LoadInt64(&r.slowdownFactor)
			if oldFactor > 1 {
				log.Printf("[%s] RECOVERED: Processing normal speed", r.ID)
			}
		}
		atomic.StoreInt64(&r.slowdownFactor, newFactor)
	}
}

// simulateProcessing simulates work with load-dependent latency
// label parameter affects processing time based on cache status
func (r *Replica) simulateProcessing(label string) (processingMs int64, loadAtStart int64, cacheHit bool) {
	// Increment load
	loadAtStart = atomic.AddInt64(&r.loadScore, 1)
	atomic.AddInt64(&r.totalHandled, 1)
	defer atomic.AddInt64(&r.loadScore, -1)

	// Check if we have this label cached
	cacheHit = r.hasLabelCached(label)

	var baseMs int
	if label == "" {
		// No label - standard processing
		baseMs = 30 + rand.Intn(40) // 30-70ms
	} else if cacheHit {
		// Cache HIT - fast processing (data already loaded)
		baseMs = 20 + rand.Intn(20) // 20-40ms
	} else {
		// Cache MISS - slow processing (need to load data first)
		baseMs = 150 + rand.Intn(100) // 150-250ms to "load" the data
		// Add to cache after processing
		defer r.addToLabelCache(label)
	}

	// Load penalty: +10ms per concurrent request
	loadPenalty := int(loadAtStart) * 10

	// Apply simulated slowdown factor
	slowdown := int(atomic.LoadInt64(&r.slowdownFactor))
	if slowdown < 1 {
		slowdown = 1
	}

	totalMs := (baseMs + loadPenalty) * slowdown
	time.Sleep(time.Duration(totalMs) * time.Millisecond)

	return int64(totalMs), loadAtStart, cacheHit
}

func (r *Replica) handleRequest(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	key := req.URL.Query().Get("key")
	if key == "" {
		key = fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	label := req.URL.Query().Get("label") // OS label like "windows", "linux", etc.

	response := RequestResponse{
		Key:        key,
		Label:      label,
		ReceivedBy: r.ID,
		Mode:       r.Mode,
	}

	var targetReplica string
	var clusterLoads map[string]int64
	var routingReason string

	if r.Mode == "gossip" && r.GossipMgr != nil {
		// Smart routing: consider both load AND label cache
		clusterLoads = r.GossipMgr.GetAllNodes()
		response.ClusterLoads = clusterLoads

		if label != "" {
			// Get nodes that have this label cached
			nodesWithLabel := r.GossipMgr.GetNodesWithLabel(label)
			
			if len(nodesWithLabel) > 0 {
				// Strategy: Find node with label AND lowest load
				// But if all cached nodes are heavily loaded, consider uncached nodes
				lowestCachedLoad := int64(999999)
				var bestCachedNode string
				for id, load := range nodesWithLabel {
					if load < lowestCachedLoad {
						lowestCachedLoad = load
						bestCachedNode = id
					}
				}

				// Also find the overall lowest loaded node
				lowestOverallLoad := int64(999999)
				var lowestOverallNode string
				for id, load := range clusterLoads {
					if load < lowestOverallLoad {
						lowestOverallLoad = load
						lowestOverallNode = id
					}
				}

				// Decision: Use cached node unless it's significantly more loaded (>3 more)
				// This models: go to cached node for speed, unless it's overwhelmed
				if lowestCachedLoad <= lowestOverallLoad+3 {
					targetReplica = bestCachedNode
					routingReason = fmt.Sprintf("cached_label=%s,load=%d@%s", label, lowestCachedLoad, bestCachedNode)
				} else {
					targetReplica = lowestOverallNode
					routingReason = fmt.Sprintf("uncached_lighter,load=%d@%s(cached=%d@%s)", lowestOverallLoad, lowestOverallNode, lowestCachedLoad, bestCachedNode)
				}
			} else {
				// No node has this label cached - send to lowest loaded node to warm it up
				lowestLoad := int64(999999)
				for id, load := range clusterLoads {
					if load < lowestLoad {
						lowestLoad = load
						targetReplica = id
					}
				}
				routingReason = fmt.Sprintf("no_cache_for=%s,warmup@%s,load=%d", label, targetReplica, lowestLoad)
			}
		} else {
			// No label - just use lowest load
			lowestLoad := int64(999999)
			for id, load := range clusterLoads {
				if load < lowestLoad {
					lowestLoad = load
					targetReplica = id
				}
			}
			routingReason = fmt.Sprintf("lowest_load=%d@%s", lowestLoad, targetReplica)
		}
	} else if r.Mode == "redis" && r.RedisClient != nil {
		// Redis central registry mode - fetch loads from Redis
		var err error
		clusterLoads, err = r.getLoadsFromRedis()
		if err != nil {
			log.Printf("[%s] Redis fetch failed: %v, falling back to local", r.ID, err)
			targetReplica = r.ID
			routingReason = "redis_error_fallback"
		} else {
			response.ClusterLoads = clusterLoads

			if label != "" {
				// Get nodes with this label from Redis
				nodesWithLabel := r.getNodesWithLabelFromRedis(label)

				if len(nodesWithLabel) > 0 {
					lowestCachedLoad := int64(999999)
					var bestCachedNode string
					for id, load := range nodesWithLabel {
						if load < lowestCachedLoad {
							lowestCachedLoad = load
							bestCachedNode = id
						}
					}

					lowestOverallLoad := int64(999999)
					var lowestOverallNode string
					for id, load := range clusterLoads {
						if load < lowestOverallLoad {
							lowestOverallLoad = load
							lowestOverallNode = id
						}
					}

					if lowestCachedLoad <= lowestOverallLoad+3 {
						targetReplica = bestCachedNode
						routingReason = fmt.Sprintf("redis_cached_label=%s,load=%d@%s", label, lowestCachedLoad, bestCachedNode)
					} else {
						targetReplica = lowestOverallNode
						routingReason = fmt.Sprintf("redis_uncached_lighter,load=%d@%s", lowestOverallLoad, lowestOverallNode)
					}
				} else {
					lowestLoad := int64(999999)
					for id, load := range clusterLoads {
						if load < lowestLoad {
							lowestLoad = load
							targetReplica = id
						}
					}
					routingReason = fmt.Sprintf("redis_no_cache_for=%s,warmup@%s,load=%d", label, targetReplica, lowestLoad)
				}
			} else {
				lowestLoad := int64(999999)
				for id, load := range clusterLoads {
					if load < lowestLoad {
						lowestLoad = load
						targetReplica = id
					}
				}
				routingReason = fmt.Sprintf("redis_lowest_load=%d@%s", lowestLoad, targetReplica)
			}
		}
	} else {
		// Consistent hash mode - hash key (or label if present) to fixed node
		hashKey := key
		if label != "" {
			hashKey = label // Hash by label for locality
		}
		targetReplica = r.Router.GetNode(hashKey)
		routingReason = fmt.Sprintf("hash(%s)->%s", hashKey, targetReplica)
	}

	response.RoutingReason = routingReason

	// If we are the target, handle locally
	if targetReplica == r.ID || targetReplica == "" {
		processingMs, loadAtHandle, cacheHit := r.simulateProcessing(label)
		response.Success = true
		response.HandledBy = r.ID
		response.Forwarded = false
		response.ProcessingMs = processingMs
		response.LatencyMs = time.Since(start).Milliseconds()
		response.LoadAtHandle = loadAtHandle
		response.CacheHit = cacheHit
	} else {
		// Forward to target replica
		forwardResp, err := r.forwardRequest(targetReplica, key, label)
		if err != nil {
			log.Printf("[%s] Forward to %s failed: %v, handling locally", r.ID, targetReplica, err)
			processingMs, loadAtHandle, cacheHit := r.simulateProcessing(label)
			response.Success = true
			response.HandledBy = r.ID
			response.Forwarded = false
			response.ProcessingMs = processingMs
			response.LatencyMs = time.Since(start).Milliseconds()
			response.LoadAtHandle = loadAtHandle
			response.CacheHit = cacheHit
			response.RoutingReason = "fallback_local"
		} else {
			response = *forwardResp
			response.ReceivedBy = r.ID
			response.Key = key
			response.Label = label
			response.Forwarded = true
			response.LatencyMs = time.Since(start).Milliseconds()
			response.ClusterLoads = clusterLoads
			response.RoutingReason = routingReason
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (r *Replica) forwardRequest(target, key, label string) (*RequestResponse, error) {
	url := fmt.Sprintf("http://%s:8080/forward?key=%s&label=%s", target, key, label)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var response RequestResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, err
	}

	return &response, nil
}

func (r *Replica) handleForward(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	key := req.URL.Query().Get("key")
	label := req.URL.Query().Get("label")

	processingMs, loadAtHandle, cacheHit := r.simulateProcessing(label)

	response := RequestResponse{
		Success:      true,
		HandledBy:    r.ID,
		ReceivedBy:   r.ID,
		Key:          key,
		Label:        label,
		CacheHit:     cacheHit,
		Forwarded:    false,
		ProcessingMs: processingMs,
		LatencyMs:    time.Since(start).Milliseconds(),
		LoadAtHandle: loadAtHandle,
		Mode:         r.Mode,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (r *Replica) handleStatus(w http.ResponseWriter, req *http.Request) {
	response := StatusResponse{
		ID:             r.ID,
		Port:           r.Port,
		LoadScore:      atomic.LoadInt64(&r.loadScore),
		TotalHandled:   atomic.LoadInt64(&r.totalHandled),
		Mode:           r.Mode,
		ClusterSize:    1,
		SlowdownFactor: atomic.LoadInt64(&r.slowdownFactor),
		CachedLabels:   r.getCachedLabels(),
	}

	if r.Mode == "gossip" && r.GossipMgr != nil {
		response.Peers = r.GossipMgr.GetPeers()
		response.ClusterSize = r.GossipMgr.NumMembers()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (r *Replica) handleHealth(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (r *Replica) handleMetrics(w http.ResponseWriter, req *http.Request) {
	type MetricsResponse struct {
		ID           string                  `json:"id"`
		Mode         string                  `json:"mode"`
		GossipMetrics *gossip.GossipMetrics  `json:"gossip_metrics,omitempty"`
	}

	response := MetricsResponse{
		ID:   r.ID,
		Mode: r.Mode,
	}

	if r.Mode == "gossip" && r.GossipMgr != nil {
		metrics := r.GossipMgr.GetMetrics()
		response.GossipMetrics = &metrics
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func main() {
	id := os.Getenv("REPLICA_ID")
	if id == "" {
		id = "replica-1"
	}

	portStr := os.Getenv("PORT")
	port := 8080
	if portStr != "" {
		port, _ = strconv.Atoi(portStr)
	}

	mode := os.Getenv("MODE")
	if mode == "" {
		mode = "gossip"
	}

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "redis:6379"
	}

	replicasStr := os.Getenv("ALL_REPLICAS")
	var allReplicas []string
	if replicasStr != "" {
		allReplicas = strings.Split(replicasStr, ",")
	} else {
		allReplicas = []string{"replica-1", "replica-2", "replica-3", "replica-4", "replica-5", "replica-6"}
	}

	rand.Seed(time.Now().UnixNano())

	replica := NewReplica(id, port, mode, allReplicas, redisAddr)
	if err := replica.Start(); err != nil {
		log.Fatalf("Failed to start replica: %v", err)
	}
}
