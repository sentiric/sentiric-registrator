// Dosya: main.go
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	consul "github.com/hashicorp/consul/api"
	"github.com/rs/zerolog"
)

//go:embed dashboard.html
var dashboardHTML string

// ServiceMapping: ContainerID -> ServiceIDs (Multiple ports support)
var (
	registryMap = make(map[string][]string)
	mapMutex    sync.RWMutex
)

type Config struct {
	NodeIP         string
	ConsulURL      string
	HostName       string
	Env            string
	LogLevel       string
	LogFormat      string
	ServiceVersion string
	TenantID       string
	HttpPort       string
	ResyncInterval int
	IgnoreDefault  string
}

func setupLogger(cfg Config) zerolog.Logger {
	level, _ := zerolog.ParseLevel(cfg.LogLevel)
	zerolog.TimeFieldFormat = time.RFC3339Nano

	logger := zerolog.New(os.Stdout).With().
		Timestamp().
		Str("schema_v", "1.0.0").
		Str("tenant_id", cfg.TenantID).
		Dict("resource", zerolog.Dict().
			Str("service.name", "registrator-service").
			Str("service.version", cfg.ServiceVersion).
			Str("service.env", cfg.Env).
			Str("host.name", cfg.HostName)).
		Logger()

	return logger.Level(level)
}

func main() {
	cfg := loadConfig()
	logger := setupLogger(cfg)

	logger.Info().Str("event", "SYSTEM_STARTUP").Msg("🚀 Registrator Ultimate v1.2.5 başlatılıyor...")

	// 1. Docker & Consul Clients
	dockerCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Fatal().Err(err).Msg("❌ Docker Client Hatası")
	}

	consulConfig := consul.DefaultConfig()
	consulConfig.Address = strings.Replace(cfg.ConsulURL, "http://", "", 1)
	consulClient, err := consul.NewClient(consulConfig)
	if err != nil {
		logger.Fatal().Err(err).Msg("❌ Consul Client Hatası")
	}

	// 2. Dashboard Server
	go startDashboard(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 3. Hybrid Sync (Event Driven + Periodic)
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.ResyncInterval) * time.Second)
		logger.Info().Int("interval_sec", cfg.ResyncInterval).Msg("🔄 Periyodik Reconciliation aktif.")
		for {
			select {
			case <-ticker.C:
				logger.Debug().Str("event", "RESYNC_TICK").Msg("Docker-Consul eşitlemesi başlıyor...")
				syncAll(ctx, dockerCli, consulClient, cfg, logger)
			case <-ctx.Done():
				return
			}
		}
	}()

	// 4. Docker Event Loop
	f := filters.NewArgs()
	f.Add("type", "container")
	msgs, errs := dockerCli.Events(ctx, types.EventsOptions{Filters: f})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logger.Info().Str("event", "EVENT_LOOP_READY").Msg("👂 Docker Socket dinleniyor...")

	for {
		select {
		case err := <-errs:
			logger.Error().Err(err).Msg("Docker Event Hatası")
		case msg := <-msgs:
			switch msg.Action {
			case "start":
				time.Sleep(2 * time.Second) // Network settle
				processContainer(ctx, dockerCli, consulClient, msg.ID, cfg, logger)
			case "die", "kill", "stop":
				deregisterContainer(consulClient, msg.ID, logger)
			}
		case <-sigChan:
			logger.Info().Str("event", "SYSTEM_SHUTDOWN").Msg("🛑 Registrator durduruluyor.")
			return
		}
	}
}

func syncAll(ctx context.Context, d *client.Client, c *consul.Client, cfg Config, logger zerolog.Logger) {
	containers, err := d.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		logger.Error().Err(err).Msg("Reconciliation list alınamadı")
		return
	}
	for _, cont := range containers {
		processContainer(ctx, d, c, cont.ID, cfg, logger)
	}
}

func processContainer(ctx context.Context, d *client.Client, c *consul.Client, containerID string, cfg Config, logger zerolog.Logger) {
	cj, err := d.ContainerInspect(ctx, containerID)
	if err != nil {
		return
	}

	// Env Parse (İlettiğin eski koddaki logic korundu)
	envMap := make(map[string]string)
	for _, e := range cj.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = strings.Trim(parts[1], "\"' ")
		}
	}

	// IGNORE Kontrolü
	if envMap["SERVICE_IGNORE"] == "true" || (cfg.IgnoreDefault == "true" && envMap["SERVICE_IGNORE"] != "false") {
		return
	}

	ignorePorts := parseIgnorePorts(envMap["SERVICE_IGNORE_PORTS"])

	// Servis İsmi
	serviceName := envMap["SERVICE_NAME"]
	if serviceName == "" {
		serviceName = strings.TrimPrefix(cj.Name, "/")
		serviceName = strings.TrimPrefix(serviceName, "sentiric-")
	}
	serviceName = strings.ToLower(serviceName)

	var registeredIDs []string
	for portProto, bindings := range cj.NetworkSettings.Ports {
		if len(bindings) == 0 {
			continue
		}

		hostPort, _ := strconv.Atoi(bindings[0].HostPort)
		if isPortIgnored(hostPort, ignorePorts) {
			continue
		}

		serviceID := fmt.Sprintf("%s-%s-%d", cfg.HostName, serviceName, hostPort)

		registration := &consul.AgentServiceRegistration{
			ID:      serviceID,
			Name:    serviceName,
			Port:    hostPort,
			Address: cfg.NodeIP,
			Tags:    []string{portProto.Proto(), "sentiric", cfg.Env},
			Meta: map[string]string{
				"container_id": containerID[:12],
				"image":        cj.Config.Image,
			},
			Check: &consul.AgentServiceCheck{
				Name:                           fmt.Sprintf("TCP Check %s:%d", serviceName, hostPort),
				TCP:                            fmt.Sprintf("%s:%d", cfg.NodeIP, hostPort),
				Interval:                       "30s", // Log kirliliğini engellemek için 30s
				Timeout:                        "5s",
				DeregisterCriticalServiceAfter: "1m",
			},
		}

		if err := c.Agent().ServiceRegister(registration); err == nil {
			registeredIDs = append(registeredIDs, serviceID)
		}
	}

	if len(registeredIDs) > 0 {
		mapMutex.Lock()
		registryMap[containerID] = registeredIDs
		mapMutex.Unlock()
	}
}

func deregisterContainer(c *consul.Client, containerID string, logger zerolog.Logger) {
	mapMutex.Lock()
	ids, ok := registryMap[containerID]
	delete(registryMap, containerID)
	mapMutex.Unlock()

	if ok {
		for _, id := range ids {
			_ = c.Agent().ServiceDeregister(id)
		}
		logger.Info().Str("event", "DEREGISTER_SUCCESS").Str("container", containerID[:12]).Msg("➖ Kayıtlar silindi.")
	}
}

func parseIgnorePorts(envVal string) []PortRange {
	var ranges []PortRange
	if envVal == "" {
		return ranges
	}
	for _, p := range strings.Split(envVal, ",") {
		p = strings.TrimSpace(p)
		if strings.Contains(p, "-") {
			bounds := strings.Split(p, "-")
			if len(bounds) == 2 {
				s, _ := strconv.Atoi(bounds[0])
				e, _ := strconv.Atoi(bounds[1])
				ranges = append(ranges, PortRange{s, e})
			}
		} else {
			port, _ := strconv.Atoi(p)
			ranges = append(ranges, PortRange{port, port})
		}
	}
	return ranges
}

type PortRange struct{ Start, End int }

func isPortIgnored(port int, ranges []PortRange) bool {
	for _, r := range ranges {
		if port >= r.Start && port <= r.End {
			return true
		}
	}
	return false
}

func startDashboard(cfg Config, logger zerolog.Logger) {
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		mapMutex.RLock()
		defer mapMutex.RUnlock()
		res := []map[string]interface{}{}
		for _, ids := range registryMap {
			for _, id := range ids {
				parts := strings.Split(id, "-")
				port := parts[len(parts)-1]
				name := strings.TrimSuffix(id, "-"+port)
				res = append(res, map[string]interface{}{"id": id, "name": name, "port": port})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, dashboardHTML)
	})

	logger.Info().Str("event", "UI_READY").Str("port", cfg.HttpPort).Msg("🖥️ Dashboard UI Aktif.")
	http.ListenAndServe(":"+cfg.HttpPort, nil)
}

func loadConfig() Config {
	hostname, _ := os.Hostname()
	return Config{
		NodeIP:         getEnv("NODE_IP", "127.0.0.1"),
		ConsulURL:      getEnv("CONSUL_URL", "http://discovery-service:8500"),
		HostName:       getEnv("NODE_HOSTNAME", hostname),
		Env:            getEnv("ENV", "production"),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
		LogFormat:      getEnv("LOG_FORMAT", "json"),
		ServiceVersion: "1.2.5",
		TenantID:       getEnv("TENANT_ID", "system"),
		HttpPort:       getEnv("REGISTRATOR_HTTP_PORT", "11090"), // Compose ile uyumlu
		ResyncInterval: getIntEnv("REGISTRATOR_RESYNC", 60),      // Periyodik eşitleme (default 60s)
		IgnoreDefault:  getEnv("SERVICE_IGNORE", "false"),
	}
}

func getEnv(k, f string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return f
}

func getIntEnv(k string, f int) int {
	v := getEnv(k, "")
	if i, err := strconv.Atoi(v); err == nil {
		return i
	}
	return f
}
