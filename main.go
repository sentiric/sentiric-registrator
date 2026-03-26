// Dosya: main.go
package main

import (
	"context"
	_ "embed" // [ZORUNLU] Statik dosyaları binary'e gömmek için
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
var dashboardHTML string // [ARCH-COMPLIANCE] HTML dosyası artık güvenli bir şekilde ayrıldı

var (
	registryMap = make(map[string]ServiceInfo)
	mapMutex    sync.RWMutex
)

type ServiceInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Port      int       `json:"port"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
}

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
}

func setupLogger(cfg Config) zerolog.Logger {
	zerolog.TimeFieldFormat = time.RFC3339Nano
	zerolog.TimestampFieldName = "ts"
	zerolog.LevelFieldName = "severity"
	zerolog.MessageFieldName = "message"

	return zerolog.New(os.Stdout).With().
		Timestamp().
		Str("schema_v", "1.0.0").
		Str("tenant_id", cfg.TenantID).
		Dict("resource", zerolog.Dict().
			Str("service.name", "registrator-service").
			Str("service.version", cfg.ServiceVersion).
			Str("service.env", cfg.Env).
			Str("host.name", cfg.HostName)).
		Logger()
}

func main() {
	cfg := loadConfig()
	logger := setupLogger(cfg)

	logger.Info().Str("event", "SYSTEM_STARTUP").Msg("🚀 Registrator (Embed UI) başlatılıyor...")

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

	go startDashboard(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	containers, _ := dockerCli.ContainerList(ctx, container.ListOptions{})
	for _, c := range containers {
		processContainer(ctx, dockerCli, consulClient, c.ID, cfg, logger)
	}

	eventFilters := filters.NewArgs()
	eventFilters.Add("type", "container")
	eventFilters.Add("event", "start")
	eventFilters.Add("event", "die")

	msgs, errs := dockerCli.Events(ctx, types.EventsOptions{Filters: eventFilters})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case err := <-errs:
			logger.Error().Err(err).Msg("Docker Event Hatası")
		case msg := <-msgs:
			if msg.Action == "start" {
				time.Sleep(1 * time.Second)
				processContainer(ctx, dockerCli, consulClient, msg.ID, cfg, logger)
			} else if msg.Action == "die" {
				deregisterContainer(consulClient, msg.ID, logger)
			}
		case <-sigChan:
			logger.Info().Str("event", "SYSTEM_SHUTDOWN").Msg("🛑 Kapatılıyor...")
			return
		}
	}
}

func processContainer(ctx context.Context, d *client.Client, c *consul.Client, containerID string, cfg Config, logger zerolog.Logger) {
	cj, err := d.ContainerInspect(ctx, containerID)
	if err != nil {
		return
	}

	envMap := make(map[string]string)
	for _, e := range cj.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = strings.Trim(parts[1], "\"' ")
		}
	}

	if envMap["SERVICE_IGNORE"] == "true" {
		return
	}

	serviceName := envMap["SERVICE_NAME"]
	if serviceName == "" {
		serviceName = strings.TrimPrefix(cj.Name, "/")
		serviceName = strings.TrimPrefix(serviceName, "sentiric-")
	}

	for _, bindings := range cj.NetworkSettings.Ports {
		if len(bindings) > 0 {
			hostPort, _ := strconv.Atoi(bindings[0].HostPort)
			serviceID := fmt.Sprintf("%s-%s-%d", cfg.HostName, serviceName, hostPort)

			reg := &consul.AgentServiceRegistration{
				ID:      serviceID,
				Name:    serviceName,
				Port:    hostPort,
				Address: cfg.NodeIP,
				Meta:    map[string]string{"container_id": containerID[:12]},
				Check: &consul.AgentServiceCheck{
					TCP:      fmt.Sprintf("%s:%d", cfg.NodeIP, hostPort),
					Interval: "15s",
				},
			}

			if err := c.Agent().ServiceRegister(reg); err == nil {
				mapMutex.Lock()
				registryMap[containerID] = ServiceInfo{
					ID: serviceID, Name: serviceName, Port: hostPort,
					Image: cj.Config.Image, CreatedAt: time.Now(),
				}
				mapMutex.Unlock()
				logger.Info().Str("event", "SERVICE_REGISTERED").Str("id", serviceID).Msg("➕ Kayıt eklendi")
			}
		}
	}
}

func deregisterContainer(c *consul.Client, containerID string, logger zerolog.Logger) {
	mapMutex.Lock()
	defer mapMutex.Unlock()

	if info, ok := registryMap[containerID]; ok {
		if err := c.Agent().ServiceDeregister(info.ID); err == nil {
			logger.Info().Str("event", "SERVICE_DEREGISTERED").Str("id", info.ID).Msg("➖ Kayıt silindi")
			delete(registryMap, containerID)
		}
	}
}

func startDashboard(cfg Config, logger zerolog.Logger) {
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		mapMutex.RLock()
		defer mapMutex.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(registryMap)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// [ARCH-COMPLIANCE] Embed değişkeni doğrudan kullanılıyor
		fmt.Fprint(w, dashboardHTML)
	})

	logger.Info().Str("event", "UI_DASHBOARD_START").Str("port", cfg.HttpPort).Msg("🖥️ Dashboard UI başlatıldı")
	if err := http.ListenAndServe(":"+cfg.HttpPort, nil); err != nil {
		logger.Error().Err(err).Msg("Dashboard başlatılamadı")
	}
}

func loadConfig() Config {
	hostname, _ := os.Hostname()
	return Config{
		NodeIP:         getEnv("NODE_IP", "127.0.0.1"),
		ConsulURL:      getEnv("CONSUL_URL", "http://discovery-service:8500"),
		HostName:       getEnv("NODE_HOSTNAME", hostname),
		Env:            getEnv("APP_ENV", "production"),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
		LogFormat:      getEnv("LOG_FORMAT", "json"),
		ServiceVersion: getEnv("SERVICE_VERSION", "1.1.1"),
		TenantID:       getEnv("TENANT_ID", "system"),
		HttpPort:       getEnv("REGISTRATOR_HTTP_PORT", "11090"),
	}
}

func getEnv(k, f string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return f
}
