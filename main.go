package main

import (
	"context"
	_ "embed"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	consul "github.com/hashicorp/consul/api"
	"github.com/rs/zerolog"
)

//go:embed dashboard.html
var dashboardHTML string

// --- GLOBAL DEĞİŞKENLER (Paylaşılan) ---
var (
	registryMap  = make(map[string][]string) // ContainerID -> []ServiceID
	mapMutex     sync.RWMutex
	dockerCli    *client.Client
	consulClient *consul.Client
)

// --- ORTAK TİPLER ---
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

type PortRange struct {
	Start int
	End   int
}

func main() {
	cfg := loadConfig()
	logger := setupLogger(cfg)

	logger.Info().Str("event", "SYSTEM_STARTUP").Msg("🚀 Registrator v1.4.0 başlatılıyor...")

	// 1. Client Hazırlığı
	dCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Fatal().Err(err).Msg("❌ Docker hatası")
	}
	dockerCli = dCli

	cCfg := consul.DefaultConfig()
	cCfg.Address = strings.Replace(cfg.ConsulURL, "http://", "", 1)
	cClient, err := consul.NewClient(cCfg)
	if err != nil {
		logger.Fatal().Err(err).Msg("❌ Consul hatası")
	}
	consulClient = cClient

	// 2. Dashboard ve API başlat
	go startDashboard(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 3. Reconciliation Loop (Eski mantık korundu)
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

	// 4. Event Loop Başlat (core.go içinde)
	go startEventLoop(ctx, cfg, logger)

	// Graceful Shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	logger.Info().Msg("🛑 Kapatılıyor...")
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
		ServiceVersion: "1.4.0",
		HttpPort:       getEnv("REGISTRATOR_HTTP_PORT", "11090"), // Senin istediğin port
		ResyncInterval: getIntEnv("REGISTRATOR_RESYNC", 60),
		TenantID:       getEnv("TENANT_ID", "system"),
	}
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
			Str("host.name", cfg.HostName)).
		Logger()
	return logger.Level(level)
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
