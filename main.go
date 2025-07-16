package main

import (
	"context"
	"encoding/json"
	"flag"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/go-hclog"
	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

var (
	namespace                = "consul_envoy_xds_server"
	serviceListUpdateCounter = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "service_list_update",
	})
	serviceUpdateCounter = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "service_update",
	})
	xdsStreamOpenCounter = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "xds_stream_open",
	})
	xdsStreamCurrentGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "xds_stream_current",
	})
)

var (
	listen         string
	httpListen     string
	logLevel       string
	logFormat      string
	enableConsul   bool
	enableNomad    bool
	consulAddr     string
	nomadAddr      string
)

func main() {
	flag.StringVar(&listen, "listen", "127.0.0.1:15000", "gRPC listen address")
	flag.StringVar(&httpListen, "http-listen", "127.0.0.1:15001", "http metrics listen address")
	flag.StringVar(&logLevel, "log-level", "info", "logging level. choose form panic,fatal,error,warn,info,debug,trace.")
	flag.StringVar(&logFormat, "log-format", "text", "log format. choose from json,text.")
	flag.BoolVar(&enableConsul, "enable-consul", true, "enable Consul service discovery")
	flag.BoolVar(&enableNomad, "enable-nomad", false, "enable Nomad service discovery")
	flag.StringVar(&consulAddr, "consul-addr", "", "Consul address (uses CONSUL_HTTP_ADDR env var if not set)")
	flag.StringVar(&nomadAddr, "nomad-addr", "", "Nomad address (uses NOMAD_ADDR env var if not set)")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	start(ctx)
}
func start(ctx context.Context) {
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		panic(err)
	}
	logger := logrus.New()
	logger.SetLevel(level)
	logger.Infof("set log level with %s", level)
	switch logFormat {
	case "json", "JSON":
		logger.SetFormatter(&logrus.JSONFormatter{})
	}

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		panic(err)
	}

	serviceUpdate := make(chan []*Service)
	
	// Initialize service watcher manager
	watcherManager := NewServiceWatcherManager(logger.WithField("component", "service-watcher"))
	
	// Add Consul watcher if enabled
	if enableConsul {
		consulConfig := api.DefaultConfig()
		if consulAddr != "" {
			consulConfig.Address = consulAddr
		}
		consulClient, err := api.NewClient(consulConfig)
		if err != nil {
			logger.WithError(err).Fatal("failed to create Consul client")
		}
		hcLogger := hclog.New(&hclog.LoggerOptions{
			Name: "consul-watch",
		})
		
		consulWatcher := NewConsulServiceWatcher(
			logger.WithField("component", "consul-watcher"),
			consulClient,
			hcLogger,
		)
		watcherManager.AddWatcher("consul", consulWatcher)
		logger.Info("Consul service discovery enabled")
	}
	
	// Add Nomad watcher if enabled
	if enableNomad {
		nomadConfig := nomadapi.DefaultConfig()
		if nomadAddr != "" {
			nomadConfig.Address = nomadAddr
		} else if addr := os.Getenv("NOMAD_ADDR"); addr != "" {
			nomadConfig.Address = addr
		}
		nomadClient, err := nomadapi.NewClient(nomadConfig)
		if err != nil {
			logger.WithError(err).Fatal("failed to create Nomad client")
		}
		
		nomadWatcher := NewNomadServiceWatcher(
			logger.WithField("component", "nomad-watcher"),
			nomadClient,
		)
		watcherManager.AddWatcher("nomad", nomadWatcher)
		logger.Info("Nomad service discovery enabled")
	}
	
	// Create HTTP server
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/", debugHandler)
	mux.HandleFunc("/debug/snapshot", snapshotHandler)
	httpServer := &http.Server{
		Addr:    httpListen,
		Handler: mux,
	}
	
	var wg sync.WaitGroup
	
	// Start service watcher
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := watcherManager.Start(); err != nil {
			logger.WithError(err).Fatal("failed to start service watchers")
		}
		// Forward updates from manager to the service update channel
		for {
			select {
			case <-ctx.Done():
				return
			case services := <-watcherManager.ServiceUpdate():
				select {
				case <-ctx.Done():
					return
				case serviceUpdate <- services:
				}
			}
		}
	}()
	
	// Start XDS server
	wg.Add(1)
	go func() {
		defer wg.Done()
		startXdsServer(ctx, lis, serviceUpdate, logger.WithField("component", "xds-server"))
	}()
	
	// Start HTTP server
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.WithField("addr", httpListen).Info("start metrics http server")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.WithError(err).Fatal("failed start metrics http server")
		}
	}()
	
	// Wait for context cancellation
	<-ctx.Done()
	logger.Info("Shutting down server...")
	
	// Shutdown HTTP server
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.WithError(err).Error("HTTP server shutdown error")
	}
	
	// Close listener
	if err := lis.Close(); err != nil {
		logger.WithError(err).Error("Listener close error")
	}
	
	// Wait for all goroutines to finish
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	
	select {
	case <-done:
		logger.Info("All services stopped gracefully")
	case <-time.After(10 * time.Second):
		logger.Warn("Timeout waiting for services to stop")
	}
}

type Service struct {
	ServiceName string
	Endpoints   []*ServiceEndpoint
}

type ServiceEndpoint struct {
	Host string
	Port uint32
}

type DebugData struct {
	Services       []*Service
	TotalServices  int
	TotalEndpoints int
	LastUpdated    string
}

func debugHandler(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles("debug_template.html")
	if err != nil {
		http.Error(w, "Template parsing error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Get services from current snapshot
	services := GetServicesFromSnapshot()

	totalEndpoints := 0
	for _, service := range services {
		totalEndpoints += len(service.Endpoints)
	}

	data := DebugData{
		Services:       services,
		TotalServices:  len(services),
		TotalEndpoints: totalEndpoints,
		LastUpdated:    time.Now().Format("2006-01-02 15:04:05"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err = tmpl.Execute(w, data)
	if err != nil {
		http.Error(w, "Template execution error: "+err.Error(), http.StatusInternalServerError)
	}
}

func snapshotHandler(w http.ResponseWriter, r *http.Request) {
	snapshot := GetCurrentSnapshot()
	if snapshot.GetVersion(resource.EndpointType) == "" {
		http.Error(w, "Snapshot not available", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	snapshotData := map[string]interface{}{
		"version":   snapshot.GetVersion(resource.EndpointType),
		"endpoints": snapshot.GetResources(resource.EndpointType),
	}

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(snapshotData); err != nil {
		http.Error(w, "JSON encoding error: "+err.Error(), http.StatusInternalServerError)
	}
}
