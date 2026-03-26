package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
)

type ServiceStatus struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Status string `json:"status"`
	Host   string `json:"host"`
}

func startDashboard(cfg Config, logger zerolog.Logger) {
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		// Consul'dan bu node'un sağlık bilgilerini çek
		checks, _, _ := consulClient.Health().Node(cfg.HostName, nil)

		mapMutex.RLock()
		defer mapMutex.RUnlock()

		res := []ServiceStatus{}
		for _, ids := range registryMap {
			for _, id := range ids {
				status := "passing"
				for _, check := range checks {
					if check.ServiceID == id {
						status = check.Status
						break
					}
				}

				p := strings.Split(id, "-")
				port, _ := strconv.Atoi(p[len(p)-1])
				res = append(res, ServiceStatus{
					ID: id, Name: strings.TrimSuffix(id, "-"+strconv.Itoa(port)),
					Port: port, Status: status, Host: cfg.HostName,
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, dashboardHTML)
	})

	logger.Info().Str("port", cfg.HttpPort).Msg("🖥️ Dashboard UI Aktif.")
	http.ListenAndServe(":"+cfg.HttpPort, nil)
}
