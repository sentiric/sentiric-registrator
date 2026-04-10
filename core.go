package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	consul "github.com/hashicorp/consul/api"
	"github.com/rs/zerolog"
)

func startEventLoop(ctx context.Context, cfg Config, logger zerolog.Logger) {
	f := filters.NewArgs()
	f.Add("type", "container")
	msgs, errs := dockerCli.Events(ctx, types.EventsOptions{Filters: f})

	for {
		select {
		case err := <-errs:
			logger.Error().Err(err).Msg("Docker socket hatası")
		case msg := <-msgs:
			if msg.Action == "start" {
				time.Sleep(2 * time.Second)
				processContainer(ctx, dockerCli, consulClient, msg.ID, cfg, logger)
			} else if msg.Action == "die" || msg.Action == "kill" || msg.Action == "stop" {
				deregisterContainer(consulClient, msg.ID, logger)
			}
		case <-ctx.Done():
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
	ignoreRanges := parseIgnorePorts(envMap["SERVICE_IGNORE_PORTS"])

	serviceName := envMap["SERVICE_NAME"]
	if serviceName == "" {
		serviceName = strings.TrimPrefix(cj.Name, "/")
		serviceName = strings.TrimPrefix(serviceName, "sentiric-")
	}
	serviceName = strings.ToLower(serviceName)

	var registeredIDs []string
	for portKey, bindings := range cj.NetworkSettings.Ports {
		if len(bindings) == 0 {
			continue
		}
		proto := portKey.Proto()
		hostPort, _ := strconv.Atoi(bindings[0].HostPort)

		if isPortIgnored(hostPort, ignoreRanges) {
			continue
		}

		serviceID := fmt.Sprintf("%s-%s-%d", cfg.HostName, serviceName, hostPort)
		registration := &consul.AgentServiceRegistration{
			ID: serviceID, Name: serviceName, Port: hostPort, Address: cfg.NodeIP,
			Tags: []string{proto, "sentiric", cfg.Env},
			Meta: map[string]string{"container_id": containerID[:12], "image": cj.Config.Image},
		}

		if proto == "tcp" {
			registration.Check = &consul.AgentServiceCheck{
				Name:     fmt.Sprintf("TCP Check %d", hostPort),
				TCP:      fmt.Sprintf("%s:%d", cfg.NodeIP, hostPort),
				Interval: "30s", Timeout: "5s", DeregisterCriticalServiceAfter: "1m",
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
		// [ARCH-COMPLIANCE] SUTS v4.2: Rutin temizlik INFO'dan DEBUG'a çekildi.
		logger.Debug().Str("event", "CONTAINER_DEREGISTERED").Str("container", containerID[:12]).Msg("➖ Kayıtlar temizlendi.")
	}
}

func isPortIgnored(port int, ranges []PortRange) bool {
	for _, r := range ranges {
		if port >= r.Start && port <= r.End {
			return true
		}
	}
	return false
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
