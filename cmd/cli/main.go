package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

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
	SlowdownFactor int64            `json:"slowdown_factor"`
	CachedLabels   []string         `json:"cached_labels,omitempty"`
	Peers          map[string]int64 `json:"peers,omitempty"`
}

type BenchmarkResult struct {
	TotalRequests   int
	SuccessCount    int
	FailedCount     int
	TotalLatencyMs  int64
	MinLatencyMs    int64
	MaxLatencyMs    int64
	AvgLatencyMs    float64
	P50LatencyMs    int64
	P95LatencyMs    int64
	P99LatencyMs    int64
	AvgLoadAtHandle float64
	HandlerDistrib  map[string]int
	ForwardedCount  int
	RequestsPerSec  float64
	Mode            string
}

func main() {
	rand.Seed(time.Now().UnixNano())

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	os.Args = append(os.Args[:1], os.Args[2:]...)

	switch cmd {
	case "request":
		cmdRequest()
	case "benchmark":
		cmdBenchmark()
	case "label-benchmark":
		cmdLabelBenchmark()
	case "status":
		cmdStatus()
	case "compare":
		cmdCompare()
	case "metrics":
		cmdMetrics()
	case "loadtest":
		cmdLoadtest()
	case "chaos":
		cmdChaos()
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Gossip vs Consistent Hash vs Redis Demo CLI

Commands:
  request         Send a single request
  benchmark       Run benchmark with concurrent requests  
  label-benchmark Run benchmark with OS labels (shows cache benefits)
  status          Get cluster status
  metrics         Show gossip protocol network overhead
  compare         Run benchmark and show comparison info
  loadtest        Run sustained load test comparing all 3 modes
  chaos           Run chaos test (kill nodes during load test)

Examples:
  ./cli request -host localhost:8081 -key mykey
  ./cli request -host localhost:8081 -key mykey -label windows
  ./cli benchmark -host localhost:8081 -requests 100 -concurrency 10
  ./cli label-benchmark -requests 200 -concurrency 20
  ./cli loadtest -duration 30 -concurrency 50
  ./cli chaos -duration 30 -concurrency 30 -kill 2
  ./cli metrics
  ./cli status -host localhost:8081
  ./cli status -all
  ./cli compare -requests 200 -concurrency 20`)
}

func cmdRequest() {
	host := flag.String("host", "localhost:8081", "Host to send request to")
	key := flag.String("key", "", "Request key (random if empty)")
	label := flag.String("label", "", "OS label (windows, linux, macos, ubuntu, debian)")
	flag.Parse()

	if *key == "" {
		*key = fmt.Sprintf("key-%d", rand.Int())
	}

	resp, err := sendRequest(*host, *key, *label)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	printResponse(resp)
}

func cmdBenchmark() {
	host := flag.String("host", "localhost:8081", "Host to send requests to")
	requests := flag.Int("requests", 100, "Total number of requests")
	concurrency := flag.Int("concurrency", 10, "Concurrent requests")
	flag.Parse()

	result := runBenchmark(*host, *requests, *concurrency)
	printBenchmarkResult(result)
}

func cmdStatus() {
	host := flag.String("host", "localhost:8081", "Host to get status from")
	all := flag.Bool("all", false, "Get status from all replicas (8081-8086)")
	flag.Parse()

	if *all {
		for port := 8081; port <= 8086; port++ {
			h := fmt.Sprintf("localhost:%d", port)
			status, err := getStatus(h)
			if err != nil {
				fmt.Printf("replica-%d: ERROR - %v\n", port-8080, err)
				continue
			}
			slowStr := ""
			if status.SlowdownFactor > 1 {
				slowStr = fmt.Sprintf(" [SLOW %dx]", status.SlowdownFactor)
			}
			fmt.Printf("replica-%d: load=%d handled=%d cluster_size=%d mode=%s%s\n",
				port-8080, status.LoadScore, status.TotalHandled, status.ClusterSize, status.Mode, slowStr)
		}
	} else {
		status, err := getStatus(*host)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		printStatus(status)
	}
}

func cmdCompare() {
	requests := flag.Int("requests", 100, "Total number of requests")
	concurrency := flag.Int("concurrency", 10, "Concurrent requests")
	flag.Parse()

	fmt.Println("=== COMPARISON: Gossip vs Consistent Hash ===")
	fmt.Println()

	status, err := getStatus("localhost:8081")
	if err != nil {
		fmt.Printf("Error connecting to cluster: %v\n", err)
		fmt.Println("Make sure docker-compose is running")
		os.Exit(1)
	}

	fmt.Printf("Current cluster mode: %s\n", status.Mode)
	fmt.Printf("Running benchmark with %d requests, %d concurrent...\n\n", *requests, *concurrency)

	result := runBenchmark("localhost:8081", *requests, *concurrency)
	printBenchmarkResult(result)

	fmt.Println()
	fmt.Println("To compare modes:")
	fmt.Println("  1. Stop current cluster: docker-compose down")
	if status.Mode == "gossip" {
		fmt.Println("  2. Start in hash mode:  MODE=hash docker-compose up --build -d")
	} else {
		fmt.Println("  2. Start in gossip mode: MODE=gossip docker-compose up --build -d")
	}
	fmt.Println("  3. Run this command again")
}

// GossipMetrics mirrors the server's GossipMetrics struct
type GossipMetrics struct {
	MessagesSent        int64   `json:"messages_sent"`
	MessagesRecv        int64   `json:"messages_recv"`
	BytesSent           int64   `json:"bytes_sent"`
	BytesRecv           int64   `json:"bytes_recv"`
	UptimeSeconds       float64 `json:"uptime_seconds"`
	MsgPerSecSent       float64 `json:"msg_per_sec_sent"`
	MsgPerSecRecv       float64 `json:"msg_per_sec_recv"`
	BytesPerSecSent     float64 `json:"bytes_per_sec_sent"`
	BytesPerSecRecv     float64 `json:"bytes_per_sec_recv"`
	GossipIntervalMs    int     `json:"gossip_interval_ms"`
	ProbeIntervalMs     int     `json:"probe_interval_ms"`
	BroadcastIntervalMs int     `json:"broadcast_interval_ms"`
}

type MetricsResponse struct {
	ID            string         `json:"id"`
	Mode          string         `json:"mode"`
	GossipMetrics *GossipMetrics `json:"gossip_metrics,omitempty"`
}

func cmdMetrics() {
	fmt.Println("=== GOSSIP PROTOCOL NETWORK OVERHEAD ===")
	fmt.Println()

	status, err := getStatus("localhost:8081")
	if err != nil {
		fmt.Printf("Error connecting to cluster: %v\n", err)
		os.Exit(1)
	}

	if status.Mode != "gossip" {
		fmt.Println("Cluster is running in HASH mode - no gossip overhead.")
		fmt.Println("Switch to gossip mode to see metrics: MODE=gossip docker-compose up --build -d")
		return
	}

	fmt.Println("Fetching metrics from all replicas...")
	fmt.Println()

	var totalMsgSent, totalMsgRecv int64
	var totalBytesSent, totalBytesRecv int64
	var totalBytesPerSecSent, totalBytesPerSecRecv float64
	var count int

	for port := 8081; port <= 8086; port++ {
		host := fmt.Sprintf("localhost:%d", port)
		metrics, err := getMetrics(host)
		if err != nil {
			fmt.Printf("replica-%d: ERROR - %v\n", port-8080, err)
			continue
		}

		if metrics.GossipMetrics == nil {
			continue
		}

		m := metrics.GossipMetrics
		count++
		totalMsgSent += m.MessagesSent
		totalMsgRecv += m.MessagesRecv
		totalBytesSent += m.BytesSent
		totalBytesRecv += m.BytesRecv
		totalBytesPerSecSent += m.BytesPerSecSent
		totalBytesPerSecRecv += m.BytesPerSecRecv

		fmt.Printf("replica-%d (uptime: %.1fs):\n", port-8080, m.UptimeSeconds)
		fmt.Printf("  Messages:  sent=%d recv=%d (%.1f/s sent, %.1f/s recv)\n",
			m.MessagesSent, m.MessagesRecv, m.MsgPerSecSent, m.MsgPerSecRecv)
		fmt.Printf("  Bytes:     sent=%s recv=%s (%.1f B/s sent, %.1f B/s recv)\n",
			formatBytes(m.BytesSent), formatBytes(m.BytesRecv), m.BytesPerSecSent, m.BytesPerSecRecv)
		fmt.Println()
	}

	if count > 0 {
		fmt.Println("=== CLUSTER TOTALS ===")
		fmt.Printf("Total Messages: sent=%d recv=%d\n", totalMsgSent, totalMsgRecv)
		fmt.Printf("Total Bytes:    sent=%s recv=%s\n", formatBytes(totalBytesSent), formatBytes(totalBytesRecv))
		fmt.Printf("Avg Bandwidth:  %.1f B/s sent, %.1f B/s recv per node\n",
			totalBytesPerSecSent/float64(count), totalBytesPerSecRecv/float64(count))
		fmt.Println()
		fmt.Println("=== GOSSIP CONFIGURATION ===")
		fmt.Println("  Gossip Interval:    200ms (peer-to-peer updates)")
		fmt.Println("  Probe Interval:     500ms (health checks)")
		fmt.Println("  Broadcast Interval: 300ms (load broadcasts)")
		fmt.Println("  Push/Pull Interval: 1000ms (full state sync)")
		fmt.Println()
		fmt.Printf("Estimated overhead per node: ~%.1f KB/min\n",
			(totalBytesPerSecSent/float64(count)+totalBytesPerSecRecv/float64(count))*60/1024)
	}
}

func getMetrics(host string) (*MetricsResponse, error) {
	url := fmt.Sprintf("http://%s/metrics", host)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result MetricsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

func formatBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	} else {
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	}
}

// cmdLabelBenchmark runs a benchmark with OS labels to demonstrate cache locality benefits
func cmdLabelBenchmark() {
	requests := flag.Int("requests", 200, "Total number of requests")
	concurrency := flag.Int("concurrency", 20, "Concurrent requests")
	flag.Parse()

	osLabels := []string{"windows", "linux", "macos", "ubuntu", "debian"}

	fmt.Println("=== LABEL BENCHMARK: Cache Locality Test ===")
	fmt.Println()
	fmt.Println("OS Labels:", osLabels)
	fmt.Println()

	status, err := getStatus("localhost:8081")
	if err != nil {
		fmt.Printf("Error connecting to cluster: %v\n", err)
		fmt.Println("Make sure docker-compose is running")
		os.Exit(1)
	}

	fmt.Printf("Current cluster mode: %s\n", status.Mode)
	fmt.Printf("Running %d requests with %d concurrency...\n\n", *requests, *concurrency)

	result := runLabelBenchmark(*requests, *concurrency, osLabels)
	printLabelBenchmarkResult(result)

	fmt.Println()
	fmt.Println("Key insight:")
	if status.Mode == "gossip" {
		fmt.Println("  Gossip mode routes to cached nodes (fast) while balancing load.")
		fmt.Println("  Over time, cache hits increase as nodes warm up.")
	} else {
		fmt.Println("  Hash mode sends same label to same node (good locality)")
		fmt.Println("  but doesn't adapt when that node is overloaded.")
	}
}

// LabelBenchmarkResult extends BenchmarkResult with cache hit stats
type LabelBenchmarkResult struct {
	*BenchmarkResult
	CacheHits       int
	CacheMisses     int
	CacheHitRate    float64
	LabelDistrib    map[string]int
}

func runLabelBenchmark(totalRequests, concurrency int, labels []string) *LabelBenchmarkResult {
	result := &LabelBenchmarkResult{
		BenchmarkResult: &BenchmarkResult{
			TotalRequests:  totalRequests,
			MinLatencyMs:   999999,
			HandlerDistrib: make(map[string]int),
		},
		LabelDistrib: make(map[string]int),
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)
	latencies := make([]int64, 0, totalRequests)
	var latencyMu sync.Mutex

	var successCount, failedCount, forwardedCount int64
	var totalLatency, totalLoad int64
	var cacheHits, cacheMisses int64

	hosts := []string{
		"localhost:8081", "localhost:8082", "localhost:8083",
		"localhost:8084", "localhost:8085", "localhost:8086",
	}

	start := time.Now()

	for i := 0; i < totalRequests; i++ {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(idx int) {
			defer wg.Done()
			defer func() { <-semaphore }()

			h := hosts[rand.Intn(len(hosts))]
			key := fmt.Sprintf("key-%d", idx)
			label := labels[rand.Intn(len(labels))]

			resp, err := sendRequest(h, key, label)
			if err != nil {
				atomic.AddInt64(&failedCount, 1)
				return
			}

			atomic.AddInt64(&successCount, 1)
			atomic.AddInt64(&totalLatency, resp.LatencyMs)
			atomic.AddInt64(&totalLoad, resp.LoadAtHandle)

			if resp.Forwarded {
				atomic.AddInt64(&forwardedCount, 1)
			}

			if resp.CacheHit {
				atomic.AddInt64(&cacheHits, 1)
			} else {
				atomic.AddInt64(&cacheMisses, 1)
			}

			latencyMu.Lock()
			latencies = append(latencies, resp.LatencyMs)
			result.HandlerDistrib[resp.HandledBy]++
			result.LabelDistrib[label]++
			if result.Mode == "" {
				result.Mode = resp.Mode
			}
			latencyMu.Unlock()
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	result.SuccessCount = int(successCount)
	result.FailedCount = int(failedCount)
	result.TotalLatencyMs = totalLatency
	result.ForwardedCount = int(forwardedCount)
	result.RequestsPerSec = float64(successCount) / elapsed.Seconds()
	result.CacheHits = int(cacheHits)
	result.CacheMisses = int(cacheMisses)
	if successCount > 0 {
		result.CacheHitRate = float64(cacheHits) / float64(successCount) * 100
	}

	if successCount > 0 {
		result.AvgLatencyMs = float64(totalLatency) / float64(successCount)
		result.AvgLoadAtHandle = float64(totalLoad) / float64(successCount)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		result.MinLatencyMs = latencies[0]
		result.MaxLatencyMs = latencies[len(latencies)-1]
		result.P50LatencyMs = latencies[len(latencies)*50/100]
		result.P95LatencyMs = latencies[len(latencies)*95/100]
		if len(latencies) > 100 {
			result.P99LatencyMs = latencies[len(latencies)*99/100]
		} else {
			result.P99LatencyMs = result.MaxLatencyMs
		}
	}

	return result
}

func printLabelBenchmarkResult(r *LabelBenchmarkResult) {
	fmt.Println("┌───────────────────────────────────────────────────────┐")
	fmt.Printf("│ Label Benchmark Results (%-6s mode)                 │\n", r.Mode)
	fmt.Println("├───────────────────────────────────────────────────────┤")
	fmt.Printf("│ Total Requests:     %-35d│\n", r.TotalRequests)
	fmt.Printf("│ Successful:         %-35d│\n", r.SuccessCount)
	fmt.Printf("│ Failed:             %-35d│\n", r.FailedCount)
	fmt.Printf("│ Forwarded:          %-35d│\n", r.ForwardedCount)
	fmt.Printf("│ Throughput:         %-35.2f│\n", r.RequestsPerSec)
	fmt.Println("├───────────────────────────────────────────────────────┤")
	fmt.Printf("│ Cache Hits:         %-35d│\n", r.CacheHits)
	fmt.Printf("│ Cache Misses:       %-35d│\n", r.CacheMisses)
	fmt.Printf("│ Cache Hit Rate:     %-34.1f%%│\n", r.CacheHitRate)
	fmt.Println("├───────────────────────────────────────────────────────┤")
	fmt.Println("│ Latency (ms):                                         │")
	fmt.Printf("│   Min:              %-35d│\n", r.MinLatencyMs)
	fmt.Printf("│   Avg:              %-35.2f│\n", r.AvgLatencyMs)
	fmt.Printf("│   P50:              %-35d│\n", r.P50LatencyMs)
	fmt.Printf("│   P95:              %-35d│\n", r.P95LatencyMs)
	fmt.Printf("│   P99:              %-35d│\n", r.P99LatencyMs)
	fmt.Printf("│   Max:              %-35d│\n", r.MaxLatencyMs)
	fmt.Println("├───────────────────────────────────────────────────────┤")
	fmt.Printf("│ Avg Load at Handle: %-35.2f│\n", r.AvgLoadAtHandle)
	fmt.Println("└───────────────────────────────────────────────────────┘")

	fmt.Println()
	fmt.Println("Handler Distribution:")
	for _, h := range []string{"replica-1", "replica-2", "replica-3", "replica-4", "replica-5", "replica-6"} {
		count := r.HandlerDistrib[h]
		pct := float64(count) / float64(r.SuccessCount) * 100
		bar := ""
		for j := 0; j < int(pct/2); j++ {
			bar += "█"
		}
		fmt.Printf("  %-10s %4d (%5.1f%%) %s\n", h, count, pct, bar)
	}
}

func sendRequest(host, key, label string) (*RequestResponse, error) {
	url := fmt.Sprintf("http://%s/request?key=%s&label=%s", host, key, label)
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result RequestResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

func getStatus(host string) (*StatusResponse, error) {
	url := fmt.Sprintf("http://%s/status", host)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

func runBenchmark(host string, totalRequests, concurrency int) *BenchmarkResult {
	result := &BenchmarkResult{
		TotalRequests:  totalRequests,
		MinLatencyMs:   999999,
		HandlerDistrib: make(map[string]int),
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)
	latencies := make([]int64, 0, totalRequests)
	var latencyMu sync.Mutex

	var successCount, failedCount, forwardedCount int64
	var totalLatency, totalLoad int64

	hosts := []string{
		"localhost:8081", "localhost:8082", "localhost:8083",
		"localhost:8084", "localhost:8085", "localhost:8086",
	}

	start := time.Now()

	for i := 0; i < totalRequests; i++ {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(idx int) {
			defer wg.Done()
			defer func() { <-semaphore }()

			h := hosts[rand.Intn(len(hosts))]
			key := fmt.Sprintf("key-%d", idx)

			resp, err := sendRequest(h, key, "")
			if err != nil {
				atomic.AddInt64(&failedCount, 1)
				return
			}

			atomic.AddInt64(&successCount, 1)
			atomic.AddInt64(&totalLatency, resp.LatencyMs)
			atomic.AddInt64(&totalLoad, resp.LoadAtHandle)

			if resp.Forwarded {
				atomic.AddInt64(&forwardedCount, 1)
			}

			latencyMu.Lock()
			latencies = append(latencies, resp.LatencyMs)
			result.HandlerDistrib[resp.HandledBy]++
			if result.Mode == "" {
				result.Mode = resp.Mode
			}
			latencyMu.Unlock()
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	result.SuccessCount = int(successCount)
	result.FailedCount = int(failedCount)
	result.TotalLatencyMs = totalLatency
	result.ForwardedCount = int(forwardedCount)
	result.RequestsPerSec = float64(successCount) / elapsed.Seconds()

	if successCount > 0 {
		result.AvgLatencyMs = float64(totalLatency) / float64(successCount)
		result.AvgLoadAtHandle = float64(totalLoad) / float64(successCount)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		result.MinLatencyMs = latencies[0]
		result.MaxLatencyMs = latencies[len(latencies)-1]
		result.P50LatencyMs = latencies[len(latencies)*50/100]
		result.P95LatencyMs = latencies[len(latencies)*95/100]
		if len(latencies) > 100 {
			result.P99LatencyMs = latencies[len(latencies)*99/100]
		} else {
			result.P99LatencyMs = result.MaxLatencyMs
		}
	}

	return result
}

func printResponse(resp *RequestResponse) {
	fmt.Println("┌─────────────────────────────────────────┐")
	fmt.Println("│ Request Result                          │")
	fmt.Println("├─────────────────────────────────────────┤")
	fmt.Printf("│ Key:            %-23s │\n", resp.Key)
	fmt.Printf("│ Mode:           %-23s │\n", resp.Mode)
	fmt.Printf("│ Received By:    %-23s │\n", resp.ReceivedBy)
	fmt.Printf("│ Handled By:     %-23s │\n", resp.HandledBy)
	fmt.Printf("│ Forwarded:      %-23v │\n", resp.Forwarded)
	fmt.Printf("│ Latency:        %-23s │\n", fmt.Sprintf("%dms", resp.LatencyMs))
	fmt.Printf("│ Processing:     %-23s │\n", fmt.Sprintf("%dms", resp.ProcessingMs))
	fmt.Printf("│ Load at Handle: %-23d │\n", resp.LoadAtHandle)
	fmt.Println("└─────────────────────────────────────────┘")

	if len(resp.ClusterLoads) > 0 {
		fmt.Println("\nCluster Loads:")
		for id, load := range resp.ClusterLoads {
			fmt.Printf("  %s: %d\n", id, load)
		}
	}
}

func printStatus(status *StatusResponse) {
	fmt.Println("┌─────────────────────────────────────────┐")
	fmt.Println("│ Replica Status                          │")
	fmt.Println("├─────────────────────────────────────────┤")
	fmt.Printf("│ ID:             %-23s │\n", status.ID)
	fmt.Printf("│ Mode:           %-23s │\n", status.Mode)
	fmt.Printf("│ Load Score:     %-23d │\n", status.LoadScore)
	fmt.Printf("│ Total Handled:  %-23d │\n", status.TotalHandled)
	fmt.Printf("│ Cluster Size:   %-23d │\n", status.ClusterSize)
	fmt.Println("└─────────────────────────────────────────┘")

	if len(status.Peers) > 0 {
		fmt.Println("\nPeer Loads:")
		for id, load := range status.Peers {
			fmt.Printf("  %s: %d\n", id, load)
		}
	}
}

func printBenchmarkResult(r *BenchmarkResult) {
	fmt.Println("┌─────────────────────────────────────────────────────────┐")
	fmt.Printf("│ Benchmark Results (%s mode)%-*s│\n", r.Mode, 33-len(r.Mode), "")
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Printf("│ Total Requests:     %-36d │\n", r.TotalRequests)
	fmt.Printf("│ Successful:         %-36d │\n", r.SuccessCount)
	fmt.Printf("│ Failed:             %-36d │\n", r.FailedCount)
	fmt.Printf("│ Forwarded:          %-36d │\n", r.ForwardedCount)
	fmt.Printf("│ Throughput:         %-36s │\n", fmt.Sprintf("%.2f req/sec", r.RequestsPerSec))
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Println("│ Latency (ms):                                           │")
	fmt.Printf("│   Min:              %-36d │\n", r.MinLatencyMs)
	fmt.Printf("│   Avg:              %-36.2f │\n", r.AvgLatencyMs)
	fmt.Printf("│   P50:              %-36d │\n", r.P50LatencyMs)
	fmt.Printf("│   P95:              %-36d │\n", r.P95LatencyMs)
	fmt.Printf("│   P99:              %-36d │\n", r.P99LatencyMs)
	fmt.Printf("│   Max:              %-36d │\n", r.MaxLatencyMs)
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Printf("│ Avg Load at Handle: %-36.2f │\n", r.AvgLoadAtHandle)
	fmt.Println("└─────────────────────────────────────────────────────────┘")

	fmt.Println("\nHandler Distribution:")
	handlers := make([]string, 0, len(r.HandlerDistrib))
	for h := range r.HandlerDistrib {
		handlers = append(handlers, h)
	}
	sort.Strings(handlers)

	for _, h := range handlers {
		count := r.HandlerDistrib[h]
		pct := float64(count) / float64(r.SuccessCount) * 100
		bar := ""
		for i := 0; i < int(pct/2); i++ {
			bar += "█"
		}
		fmt.Printf("  %-12s %4d (%5.1f%%) %s\n", h, count, pct, bar)
	}

	if len(r.HandlerDistrib) > 0 {
		avg := float64(r.SuccessCount) / float64(len(r.HandlerDistrib))
		var variance float64
		for _, count := range r.HandlerDistrib {
			diff := float64(count) - avg
			variance += diff * diff
		}
		variance /= float64(len(r.HandlerDistrib))
		fmt.Printf("\nDistribution Variance: %.2f (lower = more balanced)\n", variance)
	}
}

// LoadTestResult holds results for a sustained load test
type LoadTestResult struct {
	Mode           string
	Duration       time.Duration
	TotalRequests  int64
	SuccessCount   int64
	FailedCount    int64
	Throughput     float64 // requests per second
	AvgLatencyMs   float64
	P50LatencyMs   int64
	P95LatencyMs   int64
	P99LatencyMs   int64
	MaxLatencyMs   int64
	AvgCacheHitPct float64
}

func cmdLoadtest() {
	duration := flag.Int("duration", 30, "Test duration in seconds")
	concurrency := flag.Int("concurrency", 50, "Number of concurrent workers")
	flag.Parse()

	fmt.Println("=======================================================")
	fmt.Println("       SUSTAINED LOAD TEST: Gossip vs Redis vs Hash")
	fmt.Println("=======================================================")
	fmt.Printf("Duration: %d seconds | Concurrency: %d workers\n\n", *duration, *concurrency)

	// Check current mode
	status, err := getStatus("localhost:8081")
	if err != nil {
		fmt.Printf("Error connecting to cluster: %v\n", err)
		fmt.Println("Make sure cluster is running with: MODE=gossip docker-compose up -d")
		os.Exit(1)
	}

	fmt.Printf("Current cluster mode: %s\n\n", status.Mode)
	fmt.Println("Running load test...")
	fmt.Println()

	result := runLoadTest(*duration, *concurrency)
	printLoadTestResult(result)

	fmt.Println()
	fmt.Println("=======================================================")
	fmt.Println("                    HOW TO COMPARE")
	fmt.Println("=======================================================")
	fmt.Println()
	fmt.Println("Run this test in each mode to compare:")
	fmt.Println()
	fmt.Println("  1. GOSSIP mode (decentralized, adaptive):")
	fmt.Println("     MODE=gossip docker-compose up --build -d && sleep 5")
	fmt.Printf("     go run cmd/cli/main.go loadtest -duration %d -concurrency %d\n", *duration, *concurrency)
	fmt.Println()
	fmt.Println("  2. REDIS mode (centralized registry):")
	fmt.Println("     MODE=redis docker-compose up --build -d && sleep 5")
	fmt.Printf("     go run cmd/cli/main.go loadtest -duration %d -concurrency %d\n", *duration, *concurrency)
	fmt.Println()
	fmt.Println("  3. HASH mode (consistent hashing, no load balancing):")
	fmt.Println("     MODE=hash docker-compose up --build -d && sleep 5")
	fmt.Printf("     go run cmd/cli/main.go loadtest -duration %d -concurrency %d\n", *duration, *concurrency)
	fmt.Println()
	fmt.Println("Expected results:")
	fmt.Println("  - GOSSIP: Best throughput, lowest tail latency under load")
	fmt.Println("  - REDIS:  Slight overhead from Redis RTT, but still load-aware")
	fmt.Println("  - HASH:   Hot spots cause high tail latency when labels cluster")
}

func runLoadTest(durationSec int, concurrency int) *LoadTestResult {
	labels := []string{"windows", "linux", "macos", "ubuntu", "debian"}
	hosts := []string{"localhost:8081", "localhost:8082", "localhost:8083",
		"localhost:8084", "localhost:8085", "localhost:8086"}

	var totalRequests, successCount, failedCount, cacheHits int64
	var totalLatency int64
	latencies := make([]int64, 0, 10000)
	var latenciesMu sync.Mutex
	var mode string

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					// Pick random host and label
					host := hosts[rand.Intn(len(hosts))]
					label := labels[rand.Intn(len(labels))]
					key := fmt.Sprintf("loadtest-%d-%d", workerID, time.Now().UnixNano())

					resp, err := sendRequest(host, key, label)
					atomic.AddInt64(&totalRequests, 1)

					if err != nil {
						atomic.AddInt64(&failedCount, 1)
					} else {
						atomic.AddInt64(&successCount, 1)
						atomic.AddInt64(&totalLatency, resp.LatencyMs)
						if resp.CacheHit {
							atomic.AddInt64(&cacheHits, 1)
						}
						if mode == "" {
							mode = resp.Mode
						}

						latenciesMu.Lock()
						latencies = append(latencies, resp.LatencyMs)
						latenciesMu.Unlock()
					}
				}
			}
		}(i)
	}

	// Progress reporting
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				total := atomic.LoadInt64(&totalRequests)
				success := atomic.LoadInt64(&successCount)
				fmt.Printf("  Progress: %d requests (%d successful)\n", total, success)
			}
		}
	}()

	// Run for specified duration
	time.Sleep(time.Duration(durationSec) * time.Second)
	close(stopCh)
	ticker.Stop()

	// Wait for workers to finish current requests
	wg.Wait()

	// Calculate results
	result := &LoadTestResult{
		Mode:          mode,
		Duration:      time.Duration(durationSec) * time.Second,
		TotalRequests: totalRequests,
		SuccessCount:  successCount,
		FailedCount:   failedCount,
		Throughput:    float64(successCount) / float64(durationSec),
	}

	if successCount > 0 {
		result.AvgLatencyMs = float64(totalLatency) / float64(successCount)
		result.AvgCacheHitPct = float64(cacheHits) / float64(successCount) * 100

		// Calculate percentiles
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		result.P50LatencyMs = latencies[len(latencies)*50/100]
		result.P95LatencyMs = latencies[len(latencies)*95/100]
		result.P99LatencyMs = latencies[len(latencies)*99/100]
		result.MaxLatencyMs = latencies[len(latencies)-1]
	}

	return result
}

func printLoadTestResult(r *LoadTestResult) {
	fmt.Println("┌─────────────────────────────────────────────────────────┐")
	fmt.Printf("│ MODE: %-51s │\n", r.Mode)
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Printf("│ Duration:           %-36s │\n", r.Duration.String())
	fmt.Printf("│ Total Requests:     %-36d │\n", r.TotalRequests)
	fmt.Printf("│ Successful:         %-36d │\n", r.SuccessCount)
	fmt.Printf("│ Failed:             %-36d │\n", r.FailedCount)
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Printf("│ THROUGHPUT:         %-36s │\n", fmt.Sprintf("%.2f req/sec", r.Throughput))
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Println("│ Latency (ms):                                           │")
	fmt.Printf("│   Avg:              %-36.2f │\n", r.AvgLatencyMs)
	fmt.Printf("│   P50:              %-36d │\n", r.P50LatencyMs)
	fmt.Printf("│   P95:              %-36d │\n", r.P95LatencyMs)
	fmt.Printf("│   P99:              %-36d │\n", r.P99LatencyMs)
	fmt.Printf("│   Max:              %-36d │\n", r.MaxLatencyMs)
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Printf("│ Cache Hit Rate:     %-36s │\n", fmt.Sprintf("%.1f%%", r.AvgCacheHitPct))
	fmt.Println("└─────────────────────────────────────────────────────────┘")
}

// ChaosTestResult holds results for chaos testing
type ChaosTestResult struct {
	Mode               string
	Duration           time.Duration
	NodesKilled        int
	TotalRequests      int64
	SuccessCount       int64
	FailedCount        int64
	Throughput         float64
	AvgLatencyMs       float64
	P50LatencyMs       int64
	P95LatencyMs       int64
	P99LatencyMs       int64
	MaxLatencyMs       int64
	ErrorsDuringChaos  int64
	RecoveryTimeMs     int64
	FailedBeforeKill   int64
	FailedAfterKill    int64
	SuccessAfterKill   int64
}

func cmdChaos() {
	duration := flag.Int("duration", 30, "Test duration in seconds")
	concurrency := flag.Int("concurrency", 30, "Number of concurrent workers")
	killCount := flag.Int("kill", 2, "Number of nodes to kill during test")
	flag.Parse()

	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("          CHAOS TEST: Node Failure Resilience")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("Duration: %d seconds | Concurrency: %d | Nodes to kill: %d\n\n", *duration, *concurrency, *killCount)

	// Check current mode
	status, err := getStatus("localhost:8081")
	if err != nil {
		fmt.Printf("Error connecting to cluster: %v\n", err)
		fmt.Println("Make sure cluster is running with: MODE=gossip docker-compose up -d")
		os.Exit(1)
	}

	fmt.Printf("Current cluster mode: %s\n\n", status.Mode)

	// Run chaos test
	result := runChaosTest(*duration, *concurrency, *killCount, status.Mode)
	printChaosResult(result)

	// Restart killed nodes
	fmt.Println("\nRestarting killed nodes...")
	restartAllNodes()
	fmt.Println("All nodes restarted.")
}

func runChaosTest(durationSec, concurrency, killCount int, mode string) *ChaosTestResult {
	labels := []string{"windows", "linux", "macos", "ubuntu", "debian"}
	hosts := []string{"localhost:8081", "localhost:8082", "localhost:8083",
		"localhost:8084", "localhost:8085", "localhost:8086"}
	containers := []string{"replica-1", "replica-2", "replica-3", "replica-4", "replica-5", "replica-6"}

	var totalRequests, successCount, failedCount int64
	var totalLatency int64
	var failedBeforeKill, failedAfterKill, successAfterKill int64
	latencies := make([]int64, 0, 10000)
	var latenciesMu sync.Mutex
	var chaosStarted int64 // atomic flag

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					host := hosts[rand.Intn(len(hosts))]
					label := labels[rand.Intn(len(labels))]
					key := fmt.Sprintf("chaos-%d-%d", workerID, time.Now().UnixNano())

					resp, err := sendRequest(host, key, label)
					atomic.AddInt64(&totalRequests, 1)

					chaosActive := atomic.LoadInt64(&chaosStarted) == 1

					if err != nil {
						atomic.AddInt64(&failedCount, 1)
						if chaosActive {
							atomic.AddInt64(&failedAfterKill, 1)
						} else {
							atomic.AddInt64(&failedBeforeKill, 1)
						}
					} else {
						atomic.AddInt64(&successCount, 1)
						atomic.AddInt64(&totalLatency, resp.LatencyMs)
						if chaosActive {
							atomic.AddInt64(&successAfterKill, 1)
						}

						latenciesMu.Lock()
						latencies = append(latencies, resp.LatencyMs)
						latenciesMu.Unlock()
					}
				}
			}
		}(i)
	}

	// Phase 1: Normal operation (first third of duration)
	normalDuration := durationSec / 3
	fmt.Printf("Phase 1: Normal operation for %d seconds...\n", normalDuration)
	time.Sleep(time.Duration(normalDuration) * time.Second)

	// Phase 2: Kill nodes
	fmt.Printf("\nPhase 2: KILLING %d nodes...\n", killCount)
	killedNodes := make([]string, 0, killCount)

	// Randomly select nodes to kill (avoid replica-1 as entry point)
	shuffled := make([]int, len(containers)-1)
	for i := range shuffled {
		shuffled[i] = i + 1 // Skip replica-1 (index 0)
	}
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	for i := 0; i < killCount && i < len(shuffled); i++ {
		container := containers[shuffled[i]]
		fmt.Printf("  Killing %s...\n", container)
		if err := killNode(container); err != nil {
			fmt.Printf("  Warning: Failed to kill %s: %v\n", container, err)
		} else {
			killedNodes = append(killedNodes, container)
		}
	}

	atomic.StoreInt64(&chaosStarted, 1)
	failedAtKill := atomic.LoadInt64(&failedCount)

	// Phase 3: Continue under failure (remaining duration)
	chaosDuration := durationSec - normalDuration
	fmt.Printf("\nPhase 3: Operating under failure for %d seconds...\n", chaosDuration)

	// Progress reporting
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				total := atomic.LoadInt64(&totalRequests)
				success := atomic.LoadInt64(&successCount)
				failed := atomic.LoadInt64(&failedCount)
				fmt.Printf("  Progress: %d requests (%d ok, %d failed)\n", total, success, failed)
			}
		}
	}()

	time.Sleep(time.Duration(chaosDuration) * time.Second)
	close(stopCh)
	ticker.Stop()
	wg.Wait()

	// Calculate results
	result := &ChaosTestResult{
		Mode:             mode,
		Duration:         time.Duration(durationSec) * time.Second,
		NodesKilled:      len(killedNodes),
		TotalRequests:    totalRequests,
		SuccessCount:     successCount,
		FailedCount:      failedCount,
		Throughput:       float64(successCount) / float64(durationSec),
		FailedBeforeKill: failedBeforeKill,
		FailedAfterKill:  failedCount - failedAtKill,
		SuccessAfterKill: successAfterKill,
	}

	if successCount > 0 {
		result.AvgLatencyMs = float64(totalLatency) / float64(successCount)

		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		if len(latencies) > 0 {
			result.P50LatencyMs = latencies[len(latencies)*50/100]
			result.P95LatencyMs = latencies[len(latencies)*95/100]
			result.P99LatencyMs = latencies[len(latencies)*99/100]
			result.MaxLatencyMs = latencies[len(latencies)-1]
		}
	}

	return result
}

func killNode(container string) error {
	cmd := exec.Command("docker", "stop", container)
	return cmd.Run()
}

func restartAllNodes() {
	containers := []string{"replica-2", "replica-3", "replica-4", "replica-5", "replica-6"}
	for _, c := range containers {
		exec.Command("docker", "start", c).Run()
	}
	time.Sleep(3 * time.Second) // Wait for nodes to rejoin
}

func printChaosResult(r *ChaosTestResult) {
	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────────────────┐")
	fmt.Printf("│ CHAOS TEST RESULTS: %-37s │\n", r.Mode)
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Printf("│ Duration:           %-36s │\n", r.Duration.String())
	fmt.Printf("│ Nodes Killed:       %-36d │\n", r.NodesKilled)
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Printf("│ Total Requests:     %-36d │\n", r.TotalRequests)
	fmt.Printf("│ Successful:         %-36d │\n", r.SuccessCount)
	fmt.Printf("│ Failed:             %-36d │\n", r.FailedCount)
	successRate := float64(r.SuccessCount) / float64(r.TotalRequests) * 100
	fmt.Printf("│ Success Rate:       %-36s │\n", fmt.Sprintf("%.2f%%", successRate))
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Printf("│ THROUGHPUT:         %-36s │\n", fmt.Sprintf("%.2f req/sec", r.Throughput))
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Println("│ Failure Analysis:                                       │")
	fmt.Printf("│   Failed before kill: %-33d │\n", r.FailedBeforeKill)
	fmt.Printf("│   Failed after kill:  %-33d │\n", r.FailedAfterKill)
	fmt.Printf("│   Success after kill: %-33d │\n", r.SuccessAfterKill)
	fmt.Println("├─────────────────────────────────────────────────────────┤")
	fmt.Println("│ Latency (ms):                                           │")
	fmt.Printf("│   Avg:              %-36.2f │\n", r.AvgLatencyMs)
	fmt.Printf("│   P50:              %-36d │\n", r.P50LatencyMs)
	fmt.Printf("│   P95:              %-36d │\n", r.P95LatencyMs)
	fmt.Printf("│   P99:              %-36d │\n", r.P99LatencyMs)
	fmt.Printf("│   Max:              %-36d │\n", r.MaxLatencyMs)
	fmt.Println("└─────────────────────────────────────────────────────────┘")
}
