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

// Bellek Yönetimi: Hangi container hangi Consul ID'lerini kullanıyor?
var (
	registryMap = make(map[string][]string) // ContainerID -> []ServiceID
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
	HttpPort       string
	ResyncInterval int
	TenantID       string
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

	logger.Info().Str("event", "SYSTEM_STARTUP").Msg("🚀 Registrator v1.3.0 başlatılıyor...")

	// 1. Docker Client
	dockerCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Fatal().Err(err).Msg("❌ Docker hatası")
	}

	// 2. Consul Client
	consulConfig := consul.DefaultConfig()
	consulConfig.Address = strings.Replace(cfg.ConsulURL, "http://", "", 1)
	consulClient, err := consul.NewClient(consulConfig)
	if err != nil {
		logger.Fatal().Err(err).Msg("❌ Consul hatası")
	}

	// 3. UI Dashboard
	go startDashboard(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 4. Reconciliation Loop (Zorunlu Eşitleme)
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.ResyncInterval) * time.Second)
		for {
			select {
			case <-ticker.C:
				syncAll(ctx, dockerCli, consulClient, cfg, logger)
			case <-ctx.Done():
				return
			}
		}
	}()

	// 5. Event Loop (Anlık Yakalama)
	f := filters.NewArgs()
	f.Add("type", "container")
	msgs, errs := dockerCli.Events(ctx, types.EventsOptions{Filters: f})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case err := <-errs:
			logger.Error().Err(err).Msg("Docker socket hatası")
		case msg := <-msgs:
			if msg.Action == "start" {
				time.Sleep(2 * time.Second)
				processContainer(ctx, dockerCli, consulClient, msg.ID, cfg, logger)
			} else if msg.Action == "die" || msg.Action == "kill" {
				deregisterContainer(consulClient, msg.ID, logger)
			}
		case <-sig:
			return
		}
	}
}

func syncAll(ctx context.Context, d *client.Client, c *consul.Client, cfg Config, logger zerolog.Logger) {
	containers, _ := d.ContainerList(ctx, container.ListOptions{})
	for _, cont := range containers {
		processContainer(ctx, d, c, cont.ID, cfg, logger)
	}
}

func processContainer(ctx context.Context, d *client.Client, c *consul.Client, containerID string, cfg Config, logger zerolog.Logger) {
	cj, err := d.ContainerInspect(ctx, containerID)
	if err != nil {
		return
	}

	// ENV Analizi (Eski kodundaki mantığı tam koruyoruz)
	envMap := make(map[string]string)
	for _, e := range cj.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = strings.Trim(parts[1], "\"' ")
		}
	}

	// IGNORE Kontrolü
	if envMap["SERVICE_IGNORE"] == "true" {
		return
	}

	ignoreRanges := parseIgnorePorts(envMap["SERVICE_IGNORE_PORTS"])

	// Servis İsmi Belirle
	serviceName := envMap["SERVICE_NAME"]
	if serviceName == "" {
		serviceName = strings.TrimPrefix(cj.Name, "/")
		serviceName = strings.TrimPrefix(serviceName, "sentiric-")
	}
	serviceName = strings.ToLower(serviceName)

	var registeredIDs []string

	// Docker Port Eşleşmelerini Tara
	for portKey, bindings := range cj.NetworkSettings.Ports {
		if len(bindings) == 0 {
			continue
		}

		proto := portKey.Proto()
		hostPort, _ := strconv.Atoi(bindings[0].HostPort)

		// [KRİTİK FIX]: Ignore Kontrolü
		if isPortIgnored(hostPort, ignoreRanges) {
			continue
		}

		serviceID := fmt.Sprintf("%s-%s-%d", cfg.HostName, serviceName, hostPort)

		registration := &consul.AgentServiceRegistration{
			ID:      serviceID,
			Name:    serviceName,
			Port:    hostPort,
			Address: cfg.NodeIP,
			Tags:    []string{proto, "sentiric", cfg.Env},
			Meta: map[string]string{
				"container_id": containerID[:12],
				"image":        cj.Config.Image,
			},
		}

		// [PROFESYONEL MANTIK]: Sadece TCP portlarına Health Check ekle
		// UDP portları (SIP/RTP) için TCP check yapmak logları kirletir.
		if proto == "tcp" {
			registration.Check = &consul.AgentServiceCheck{
				Name:                           fmt.Sprintf("TCP Check %d", hostPort),
				TCP:                            fmt.Sprintf("%s:%d", cfg.NodeIP, hostPort),
				Interval:                       "30s",
				Timeout:                        "5s",
				DeregisterCriticalServiceAfter: "1m",
			}
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
		logger.Debug().Str("container", containerID[:12]).Msg("➖ Kayıtlar temizlendi.")
	}
}

type PortRange struct{ Start, End int }

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
				p := strings.Split(id, "-")
				port := p[len(p)-1]
				res = append(res, map[string]interface{}{
					"id": id, "name": strings.TrimSuffix(id, "-"+port), "port": port, "proto": "tcp/udp",
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
	http.ListenAndServe(":"+cfg.HttpPort, nil)
}

func loadConfig() Config {
	h, _ := os.Hostname()
	return Config{
		NodeIP:         getEnv("NODE_IP", "127.0.0.1"),
		ConsulURL:      getEnv("CONSUL_URL", "http://discovery-service:8500"),
		HostName:       getEnv("NODE_HOSTNAME", h),
		Env:            getEnv("ENV", "production"),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
		LogFormat:      getEnv("LOG_FORMAT", "json"),
		ServiceVersion: "1.3.0",
		HttpPort:       getEnv("REGISTRATOR_HTTP_PORT", "11090"),
		ResyncInterval: getIntEnv("REGISTRATOR_RESYNC", 60),
		TenantID:       getEnv("TENANT_ID", "system"),
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
	i, err := strconv.Atoi(v)
	if err == nil {
		return i
	}
	return f
}
