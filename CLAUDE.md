# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

This is a Go application that acts as an Envoy xDS (Extended Discovery Service) server, bridging Consul service discovery with Envoy proxy configuration. It watches Consul services and their health checks, then provides Envoy with endpoint configurations via the xDS API.

## Architecture

The application consists of three main components:

### 1. Consul Service Registry (`consul_watch.go`)
- **ServiceRegistry**: Manages a collection of watched services from Consul
- **WatchedService**: Monitors individual services and their health checks
- Uses Consul's blocking queries to watch for service changes in real-time
- Filters out unhealthy services (Critical/Maintenance status)
- Handles both service registration/deregistration and health check updates

### 2. Envoy xDS Server (`envoy_xds.go`)
- Implements Envoy's Endpoint Discovery Service (EDS) protocol
- Uses go-control-plane library for xDS server implementation
- Generates cluster load assignments from Consul service data
- Maintains snapshot cache for efficient endpoint updates
- All nodes use a single "default" node hash for simplicity

### 3. Main Application (`main.go`)
- Coordinates the Consul watcher and xDS server
- Provides Prometheus metrics endpoint for monitoring
- Handles graceful service coordination with channels

## Key Data Flow

1. Consul service changes trigger updates in ServiceRegistry
2. ServiceRegistry emits consolidated service updates via channels
3. XDS server receives updates and generates new snapshots
4. Envoy clients receive endpoint updates through xDS protocol

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

### Running Tests
```bash
# Run E2E tests (requires Docker)
go test -v ./...
```

The E2E test uses Docker Compose to start Consul and Envoy containers, registers test services, and validates the complete integration.

### Development Environment
Set `CONSUL_HTTP_ADDR` environment variable to connect to a specific Consul instance:
```bash
export CONSUL_HTTP_ADDR=http://localhost:8500
```

## Monitoring
- Prometheus metrics available at `/metrics` endpoint
- Key metrics: service updates, xDS stream connections, service list changes
- Structured logging with configurable levels and formats

## Dependencies
- Uses go-control-plane for Envoy xDS implementation
- Consul API client for service discovery
- Prometheus for metrics collection
- logrus for structured logging