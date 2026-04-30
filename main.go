package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
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

func collectVitals(dockerClient *client.Client) (*Vitals, error) {
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

	containers := []ContainerInfo{}
	if dockerClient != nil {
		result, err := dockerClient.ContainerList(
			context.Background(),
			client.ContainerListOptions{All: true},
		)
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
					State:  ctr.ID,
				})
			}
		} else {
			panic(err)
		}
	}

	return &Vitals{
		Timestamp:   now.Unix(),
		CPU:         cpuPercent[0],
		MemUsedMB:   vmStat.Used / 1024 / 1024, //metric or something
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

		log.Printf("Client connected: %s", r.RemoteAddr)

		for {
			vitals, err := collectVitals(dockerClient)
			if err != nil {
				log.Printf("Error collecting vitals: %v", err)
				conn.Close(websocket.StatusInternalError, "collection failed")
				return
			}

			err = wsjson.Write(r.Context(), conn, vitals)
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

func main() {
	dockerClient, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Printf("Warning: Docker unavailable: %v", err)
		dockerClient = nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler(dockerClient))
	mux.HandleFunc("/health", healthHandler)

	log.Println("Vitals API listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
