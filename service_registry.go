package main

import (
	"sort"
	"sync"
	"github.com/sirupsen/logrus"
)

// ServiceWatcher defines the interface for service discovery backends
type ServiceWatcher interface {
	Start() error
	Stop()
	ServiceUpdate() <-chan []*Service
}

// ServiceWatcherManager manages multiple service watchers and merges their outputs
type ServiceWatcherManager struct {
	sync.Mutex
	logger         *logrus.Entry
	watchers       map[string]ServiceWatcher // keyed by watcher type (consul, nomad)
	servicesByType map[string]map[string]*Service // provider -> service_name -> service
	updates        chan []*Service
	stop           chan struct{}
}

func NewServiceWatcherManager(logger *logrus.Entry) *ServiceWatcherManager {
	return &ServiceWatcherManager{
		logger:         logger,
		watchers:       make(map[string]ServiceWatcher),
		servicesByType: make(map[string]map[string]*Service),
		updates:        make(chan []*Service, 10),
		stop:           make(chan struct{}),
	}
}

func (m *ServiceWatcherManager) AddWatcher(watcherType string, watcher ServiceWatcher) {
	m.Lock()
	defer m.Unlock()
	m.watchers[watcherType] = watcher
	m.servicesByType[watcherType] = make(map[string]*Service)
}

func (m *ServiceWatcherManager) Start() error {
	for watcherType, watcher := range m.watchers {
		if err := watcher.Start(); err != nil {
			m.logger.WithError(err).WithField("type", watcherType).Error("failed to start service watcher")
			return err
		}
		go m.watchUpdates(watcherType, watcher)
	}
	return nil
}

func (m *ServiceWatcherManager) watchUpdates(watcherType string, watcher ServiceWatcher) {
	for {
		select {
		case services := <-watcher.ServiceUpdate():
			m.updateServices(watcherType, services)
		case <-m.stop:
			return
		}
	}
}

func (m *ServiceWatcherManager) updateServices(watcherType string, services []*Service) {
	m.Lock()
	defer m.Unlock()
	
	// Update the services for this watcher type
	m.servicesByType[watcherType] = make(map[string]*Service)
	for _, service := range services {
		m.servicesByType[watcherType][service.ServiceName] = service
	}
	
	// Merge all services from all providers
	merged := m.mergeServices()
	
	select {
	case m.updates <- merged:
	default:
		m.logger.Warn("service update channel is full, skipping update")
	}
}

func (m *ServiceWatcherManager) mergeServices() []*Service {
	serviceMap := make(map[string]*Service)
	
	// Merge services from all providers
	// If services have the same name, merge their endpoints
	for providerType, services := range m.servicesByType {
		for serviceName, service := range services {
			if existing, exists := serviceMap[serviceName]; exists {
				// Merge endpoints from multiple providers
				allEndpoints := make([]*ServiceEndpoint, 0, len(existing.Endpoints)+len(service.Endpoints))
				allEndpoints = append(allEndpoints, existing.Endpoints...)
				allEndpoints = append(allEndpoints, service.Endpoints...)
				
				existing.Endpoints = allEndpoints
				m.logger.WithField("service", serviceName).WithField("provider", providerType).Debug("merged service endpoints")
			} else {
				// Copy service to avoid reference issues
				serviceCopy := &Service{
					ServiceName: service.ServiceName,
					Endpoints:   make([]*ServiceEndpoint, len(service.Endpoints)),
				}
				copy(serviceCopy.Endpoints, service.Endpoints)
				serviceMap[serviceName] = serviceCopy
			}
		}
	}
	
	// Convert map to slice and sort
	result := make([]*Service, 0, len(serviceMap))
	for _, service := range serviceMap {
		result = append(result, service)
	}
	
	sort.Slice(result, func(i, j int) bool {
		return result[i].ServiceName < result[j].ServiceName
	})
	
	return result
}

func (m *ServiceWatcherManager) ServiceUpdate() <-chan []*Service {
	return m.updates
}

func (m *ServiceWatcherManager) Stop() {
	close(m.stop)
	for _, watcher := range m.watchers {
		watcher.Stop()
	}
	close(m.updates)
}