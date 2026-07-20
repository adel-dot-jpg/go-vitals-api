package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/moby/moby/client"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	gopsnet "github.com/shirou/gopsutil/v3/net"
)

type ContainerInfo struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status string `json:"status"`
	State  string `json:"state"`
}

type NetworkStats struct {
	BytesSent uint64  `json:"bytes_sent"`
	BytesRecv uint64  `json:"bytes_recv"`
	MBpsSent  float64 `json:"mbps_sent"`
	MBpsRecv  float64 `json:"mbps_recv"`
}

type Vitals struct {
	Timestamp   int64           `json:"timestamp"`
	CPU         float64         `json:"cpu_percent"`
	MemUsedMB   uint64          `json:"mem_used_mb"`
	MemTotalMB  uint64          `json:"mem_total_mb"`
	MemPercent  float64         `json:"mem_percent"`
	DiskUsedGB  uint64          `json:"disk_used_gb"`
	DiskTotalGB uint64          `json:"disk_total_gb"`
	DiskPercent float64         `json:"disk_percent"`
	Network     NetworkStats    `json:"network"`
	Containers  []ContainerInfo `json:"containers"`
}

var ( // used to calculate MB/s across vitals calls
	prevNet     gopsnet.IOCountersStat
	prevNetTime time.Time
)

var ( // benchmarks
	activeConnections int64
	totalConnections  int64
)

// --- Uptime tracking ---

type UptimeTracker struct {
	mu            sync.Mutex
	sessionStart  time.Time
	firstEverSeen time.Time
	cumulativeSec float64
	filePath      string
}

type uptimeState struct {
	FirstEverSeen time.Time `json:"first_ever_seen"`
	CumulativeSec float64   `json:"cumulative_sec"`
}

func NewUptimeTracker(path string) *UptimeTracker {
	t := &UptimeTracker{sessionStart: time.Now(), filePath: path}

	data, err := os.ReadFile(path)
	if err == nil {
		var state uptimeState
		if json.Unmarshal(data, &state) == nil {
			t.firstEverSeen = state.FirstEverSeen
			t.cumulativeSec = state.CumulativeSec
		}
	}
	if t.firstEverSeen.IsZero() {
		t.firstEverSeen = time.Now()
	}
	return t
}

// Checkpoint folds the current session's elapsed time into the cumulative
// total and persists it to disk. Call this periodically so a crash only
// loses the time since the last checkpoint, not the whole session.
func (t *UptimeTracker) Checkpoint() {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	t.cumulativeSec += now.Sub(t.sessionStart).Seconds()
	t.sessionStart = now

	state := uptimeState{FirstEverSeen: t.firstEverSeen, CumulativeSec: t.cumulativeSec}
	data, err := json.Marshal(state)
	if err != nil {
		log.Printf("uptime marshal error: %v", err)
		return
	}
	if err := os.WriteFile(t.filePath, data, 0644); err != nil {
		log.Printf("uptime write error: %v", err)
	}
}

func (t *UptimeTracker) Stats() (sessionUptime, cumulativeUptime time.Duration, uptimePercent float64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	sessionUptime = time.Since(t.sessionStart)
	cumulativeUptime = time.Duration(t.cumulativeSec)*time.Second + sessionUptime
	totalWallClock := time.Since(t.firstEverSeen).Seconds()
	if totalWallClock > 0 {
		uptimePercent = (cumulativeUptime.Seconds() / totalWallClock) * 100
	}
	return
}

var uptimeTracker *UptimeTracker

// --- Vitals collection ---

func collectVitals(dockerClient *client.Client) (*Vitals, error) {
	start := time.Now()
	defer func() {
		log.Printf("collectVitals total: %v", time.Since(start))
	}()

	cpuPercent, err := cpu.Percent(500*time.Millisecond, false)
	if err != nil {
		return nil, err
	}

	vmStat, err := mem.VirtualMemory()
	if err != nil {
		return nil, err
	}

	diskStat, err := disk.Usage("/")
	if err != nil {
		return nil, err
	}

	netStats, err := gopsnet.IOCounters(false)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	current := netStats[0]
	var mbpsSent, mbpsRecv float64

	if !prevNetTime.IsZero() {
		elapsed := now.Sub(prevNetTime).Seconds()
		mbpsSent = float64(current.BytesSent-prevNet.BytesSent) / elapsed / 1024 / 1024
		mbpsRecv = float64(current.BytesRecv-prevNet.BytesRecv) / elapsed / 1024 / 1024
	}
	prevNet = current
	prevNetTime = now

	dockerStart := time.Now()
	containers := []ContainerInfo{}
	if dockerClient != nil {
		result, err := dockerClient.ContainerList(
			context.Background(),
			client.ContainerListOptions{All: true},
		)
		log.Printf("docker ContainerList: %v", time.Since(dockerStart))
		if err == nil {
			for _, ctr := range result.Items {
				name := ""
				if len(ctr.Names) > 0 {
					name = ctr.Names[0][1:]
				}
				containers = append(containers, ContainerInfo{
					Name:   name,
					Image:  ctr.Image,
					Status: ctr.Status,
					State:  string(ctr.State),
				})
			}
		} else {
			panic(err)
		}
	}

	return &Vitals{
		Timestamp:   now.Unix(),
		CPU:         cpuPercent[0],
		MemUsedMB:   vmStat.Used / 1024 / 1024,
		MemTotalMB:  vmStat.Total / 1024 / 1024,
		MemPercent:  vmStat.UsedPercent,
		DiskUsedGB:  diskStat.Used / 1024 / 1024 / 1024,
		DiskTotalGB: diskStat.Total / 1024 / 1024 / 1024,
		DiskPercent: diskStat.UsedPercent,
		Network: NetworkStats{
			BytesSent: current.BytesSent,
			BytesRecv: current.BytesRecv,
			MBpsSent:  mbpsSent,
			MBpsRecv:  mbpsRecv,
		},
		Containers: containers,
	}, nil
}

func wsHandler(dockerClient *client.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{
				"vitals.adelfaruque.me",
				"adelfaruque.me",
				"www.adelfaruque.me",
				"*.vercel.app",
				"localhost:*",
			},
		})
		if err != nil {
			log.Printf("WebSocket accept error: %v", err)
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "closed")

		atomic.AddInt64(&activeConnections, 1)
		atomic.AddInt64(&totalConnections, 1)
		defer atomic.AddInt64(&activeConnections, -1)

		log.Printf("Client connected: %s", r.RemoteAddr)

		for {
			collectStart := time.Now()
			vitals, err := collectVitals(dockerClient)
			collectDuration := time.Since(collectStart)

			if err != nil {
				log.Printf("Error collecting vitals: %v", err)
				conn.Close(websocket.StatusInternalError, "collection failed")
				return
			}

			writeStart := time.Now()
			err = wsjson.Write(r.Context(), conn, vitals)
			writeDuration := time.Since(writeStart)

			log.Printf("collect=%v write=%v active_conns=%d",
				collectDuration, writeDuration, atomic.LoadInt64(&activeConnections))

			if err != nil {
				log.Printf("Client disconnected: %s", r.RemoteAddr)
				return
			}

			time.Sleep(2 * time.Second)
		}
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	sess, cum, pct := uptimeTracker.Stats()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"active_connections":    atomic.LoadInt64(&activeConnections),
		"total_connections":     atomic.LoadInt64(&totalConnections),
		"session_uptime_sec":    sess.Seconds(),
		"cumulative_uptime_sec": cum.Seconds(),
		"uptime_percent":        pct,
	})
}

func main() {
	dockerClient, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Printf("Warning: Docker unavailable: %v", err)
		dockerClient = nil
	}

	if err := os.MkdirAll("/var/lib/vitals-api", 0755); err != nil {
		log.Printf("Warning: could not create uptime data dir: %v", err)
	}
	uptimeTracker = NewUptimeTracker("/var/lib/vitals-api/uptime.json")

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		for range ticker.C {
			uptimeTracker.Checkpoint()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler(dockerClient))
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/stats", statsHandler)

	log.Println("Vitals API listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
