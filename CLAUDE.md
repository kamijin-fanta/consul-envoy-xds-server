# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

This is a Go application that acts as an Envoy xDS (Extended Discovery Service) server, bridging multiple service discovery backends (Consul and Nomad Services) with Envoy proxy configuration. It watches services and their health checks from configured providers, then provides Envoy with endpoint configurations via the xDS API.

## Architecture

The application consists of four main components:

### 1. Service Watcher Management (`service_registry.go`)
- **ServiceWatcher**: Interface for service discovery backends
- **ServiceWatcherManager**: Manages multiple service watchers and merges their outputs
- Supports simultaneous operation of multiple service discovery providers
- Merges services with the same name from different providers by combining their endpoints
- Provides unified service updates to the xDS server

### 2. Consul Service Watcher (`consul_watch.go`)
- **ConsulServiceWatcher**: Manages a collection of watched services from Consul
- **WatchedService**: Monitors individual services and their health checks
- Uses Consul's blocking queries to watch for service changes in real-time
- Filters out unhealthy services (Critical/Maintenance status)
- Handles both service registration/deregistration and health check updates

### 3. Nomad Service Watcher (`nomad_watch.go`)
- **NomadServiceWatcher**: Manages service discovery from Nomad's native service registry
- Uses Nomad API's blocking queries to watch for service changes in real-time
- Monitors `/v1/services` for service list changes and `/v1/service/:name` for service details
- Automatically discovers and tracks new services as they are registered

### 4. Envoy xDS Server (`envoy_xds.go`)
- Implements Envoy's Endpoint Discovery Service (EDS) protocol
- Uses go-control-plane library for xDS server implementation
- Generates cluster load assignments from merged service data
- Maintains snapshot cache for efficient endpoint updates
- All nodes use a single "default" node hash for simplicity

### 5. Main Application (`main.go`)
- Coordinates multiple service watchers and xDS server
- Provides Prometheus metrics endpoint for monitoring
- Handles graceful service coordination with channels

## Key Data Flow

1. Service changes in Consul and/or Nomad trigger updates in their respective watchers
2. ServiceWatcherManager receives updates from all active watchers
3. ServiceWatcherManager merges services from all providers and emits consolidated updates
4. XDS server receives merged updates and generates new snapshots
5. Envoy clients receive endpoint updates through xDS protocol

## Service Discovery Providers

### Consul
- Enabled by default (`--enable-consul=true`)
- Supports health check filtering (excludes Critical/Maintenance services)
- Uses Consul's native blocking queries for real-time updates
- Can be configured via `--consul-addr` or `CONSUL_HTTP_ADDR` environment variable

### Nomad Services
- Disabled by default (`--enable-nomad=false`)
- Uses Nomad's native service registry (introduced in Nomad 1.3+)
- Uses Nomad API's blocking queries for real-time updates
- Can be configured via `--nomad-addr` or `NOMAD_ADDR` environment variable
- No health check filtering (relies on Nomad's service health status)

## Common Development Commands

### Building and Running
```bash
go build -o consul-envoy-xds-server
./consul-envoy-xds-server
```

### Command Line Options
- `-listen`: gRPC listen address (default: 127.0.0.1:15000)
- `-http-listen`: HTTP metrics listen address (default: 127.0.0.1:15001)
- `-log-level`: Logging level (panic,fatal,error,warn,info,debug,trace)
- `-log-format`: Log format (json,text)
- `-enable-consul`: Enable Consul service discovery (default: true)
- `-enable-nomad`: Enable Nomad service discovery (default: false)
- `-consul-addr`: Consul address (uses CONSUL_HTTP_ADDR env var if not set)
- `-nomad-addr`: Nomad address (uses NOMAD_ADDR env var if not set)

### Running Tests
```bash
# Run E2E tests (requires Docker)
go test -v ./...
```

The E2E test uses Docker Compose to start Consul and Envoy containers, registers test services, and validates the complete integration.

### Development Environment
Set environment variables to connect to specific service discovery instances:
```bash
# For Consul
export CONSUL_HTTP_ADDR=http://localhost:8500

# For Nomad
export NOMAD_ADDR=http://localhost:4646
```

### Usage Examples
```bash
# Use only Consul (default)
./consul-envoy-xds-server

# Use only Nomad
./consul-envoy-xds-server -enable-consul=false -enable-nomad=true

# Use both Consul and Nomad
./consul-envoy-xds-server -enable-nomad=true

# Use custom addresses
./consul-envoy-xds-server -consul-addr=http://consul.example.com:8500 -nomad-addr=http://nomad.example.com:4646 -enable-nomad=true
```

## Monitoring
- Prometheus metrics available at `/metrics` endpoint
- Key metrics: service updates, xDS stream connections, service list changes
- Structured logging with configurable levels and formats

## Dependencies
- Uses go-control-plane for Envoy xDS implementation
- Consul API client for Consul service discovery
- Nomad API client for Nomad service discovery
- Prometheus for metrics collection
- logrus for structured logging

## Service Merging Behavior
When both Consul and Nomad are enabled and services with the same name are found in both providers:
- Endpoints from both providers are combined into a single service
- All endpoints are included in the xDS configuration sent to Envoy
- This allows for gradual migration between service discovery systems
- Services are uniquely identified by name, not by provider