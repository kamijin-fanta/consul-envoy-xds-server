package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/hashicorp/consul/api"
	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// Start docker-compose
	dockerComposeCmd := exec.Command("docker", "compose", "--file", "./test/docker-compose.yaml", "up", "-d")
	err := dockerComposeCmd.Run()
	if err != nil {
		fmt.Printf("Failed to start docker-compose: %v\n", err)
		os.Exit(1)
	}

	// Wait for services to be ready
	time.Sleep(5 * time.Second)

	// Run tests
	code := m.Run()

	// Cleanup
	cleanupCmd := exec.Command("docker", "compose", "--file", "./test/docker-compose.yaml", "down")
	cleanupCmd.Run()

	os.Exit(code)
}

func TestE2EConsul(t *testing.T) {

	//assert := assert.New(t)
	require := require.New(t)

	{
		listen = "127.0.0.1:16000"
		httpListen = "127.0.0.1:16001"
		//logLevel = "warning"
		logLevel = "debug"
		logFormat = "text"
		enableConsul = true
		enableNomad = false
		consulAddr = ""
		nomadAddr = ""
	}
	var (
		envoyAddr      = "http://127.0.0.1:16101"
		envoyAdminAddr = "http://127.0.0.1:16102"
		consulApiAddr  = "127.0.0.1:16201"
		consulApi      = "http://127.0.0.1:16201"
	)
	require.Nil(os.Setenv("CONSUL_HTTP_ADDR", consulApi))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		start(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	dummyOkListener, err := net.Listen("tcp", "127.0.0.1:0")
	dummyOkServer := &dummyServer{status: 200}
	require.Nil(err)
	go func() {
		err := dummyOkServer.StartDummyHttpServer(dummyOkListener)
		require.Nil(err)
	}()
	dummyNgListener, err := net.Listen("tcp", "127.0.0.1:0")
	dummyNgServer := &dummyServer{status: 400}
	require.Nil(err)
	go func() {
		err := dummyNgServer.StartDummyHttpServer(dummyNgListener)
		require.Nil(err)
	}()
	var (
		dummyOkTcpAddr = dummyOkListener.Addr().(*net.TCPAddr)
		dummyNgTcpAddr = dummyNgListener.Addr().(*net.TCPAddr)
		dummyOkAddr    = fmt.Sprintf("http://%s/", dummyOkTcpAddr)
		dummyNgAddr    = fmt.Sprintf("http://%s/", dummyNgTcpAddr)
	)

	t.Logf("dummy server ok: %s ng: %s", dummyOkAddr, dummyNgAddr)

	// wait for startup services
	err = waitForUrl(envoyAdminAddr+"/ready", 200)
	require.Nil(err)
	err = waitForUrl(consulApi+"/v1/status/leader", 200)
	require.Nil(err)
	t.Logf("test service is healthy")

	consulConf := api.DefaultConfig()
	consulConf.Address = consulApiAddr
	consulClient, err := api.NewClient(consulConf)
	require.Nil(err)
	{
		successService := &api.AgentServiceRegistration{
			ID:      "success-service",
			Name:    "success-service",
			Address: dummyOkTcpAddr.IP.String(),
			Port:    dummyOkTcpAddr.Port,
			Check: &api.AgentServiceCheck{
				CheckID:  "success_service_check_",
				Name:     "check",
				HTTP:     dummyOkAddr,
				Interval: "3s",
				Timeout:  "5s",
			},
		}
		err = consulClient.Agent().ServiceRegister(successService)
		require.Nil(err)
		defer consulClient.Agent().ServiceDeregister("success-service")
	}
	{
		failService := &api.AgentServiceRegistration{
			ID:      "fail-service",
			Name:    "fail-service",
			Address: dummyNgTcpAddr.IP.String(),
			Port:    dummyNgTcpAddr.Port,
			Check: &api.AgentServiceCheck{
				CheckID:  "fail_service_check",
				Name:     "check",
				HTTP:     dummyNgAddr,
				Interval: "3s",
				Timeout:  "5s",
			},
		}
		err = consulClient.Agent().ServiceRegister(failService)
		require.Nil(err)
		defer consulClient.Agent().ServiceDeregister("fail-service")
	}

	err = waitForUrl(envoyAddr+"/success-service/", 200)
	require.Nil(err)

	time.Sleep(3 * time.Second)

	err = waitForUrl(envoyAddr+"/fail-service/", 503) // expect: 503 no healthy upstream
	require.Nil(err)

	dummyOkServer.SetStatusCode(400)
	err = waitForUrl(envoyAddr+"/success-service/", 503) // expect: 503 no healthy upstream
	require.Nil(err)

	dummyOkServer.SetStatusCode(200)
	err = waitForUrl(envoyAddr+"/success-service/", 200)
	require.Nil(err)
}

func TestE2ENomad(t *testing.T) {

	require := require.New(t)

	{
		listen = "127.0.0.1:16000"
		httpListen = "127.0.0.1:16001"
		logLevel = "debug"
		logFormat = "text"
		enableConsul = false
		enableNomad = true
		consulAddr = ""
		nomadAddr = ""
	}
	var (
		envoyAddr      = "http://127.0.0.1:16101"
		envoyAdminAddr = "http://127.0.0.1:16102"
		nomadApi       = "http://127.0.0.1:16301"
	)
	require.Nil(os.Setenv("NOMAD_ADDR", nomadApi))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		start(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// Create Nomad client for job management
	nomadConf := nomadapi.DefaultConfig()
	nomadConf.Address = nomadApi
	nomadClient, err := nomadapi.NewClient(nomadConf)
	require.Nil(err)

	defer func() {
		// Stop Nomad jobs before shutdown
		_, _, err := nomadClient.Jobs().Deregister("success-service-test", true, nil)
		if err != nil {
			t.Logf("Failed to stop success-service-test job: %v", err)
		}
		_, _, err = nomadClient.Jobs().Deregister("fail-service-test", true, nil)
		if err != nil {
			t.Logf("Failed to stop fail-service-test job: %v", err)
		}

		// Wait for jobs to be stopped
		time.Sleep(2 * time.Second)
	}()

	dummyOkListener, err := net.Listen("tcp", "127.0.0.1:0")
	dummyOkServer := &dummyServer{status: 200}
	require.Nil(err)
	go func() {
		err := dummyOkServer.StartDummyHttpServer(dummyOkListener)
		require.Nil(err)
	}()
	dummyNgListener, err := net.Listen("tcp", "127.0.0.1:0")
	dummyNgServer := &dummyServer{status: 400}
	require.Nil(err)
	go func() {
		err := dummyNgServer.StartDummyHttpServer(dummyNgListener)
		require.Nil(err)
	}()
	var (
		dummyOkTcpAddr = dummyOkListener.Addr().(*net.TCPAddr)
		dummyNgTcpAddr = dummyNgListener.Addr().(*net.TCPAddr)
	)

	// wait for startup services
	err = waitForUrl(envoyAdminAddr+"/ready", 200)
	require.Nil(err)
	err = waitForUrl(nomadApi+"/v1/status/leader", 200)
	require.Nil(err)
	t.Logf("test service is healthy")

	// Create minimal job definitions that register services pointing to our dummy servers
	successJobHCL := fmt.Sprintf(`
job "success-service-test" {
  datacenters = ["dc1"]
  type = "service"

  group "web" {
    count = 1

    network {
      port "http" {
        static = %d
      }
    }

    task "dummy" {
			driver = "docker"

      config {
        image   = "alpine:latest"
        command = "sleep"
        args    = ["600"]
      }

      service {
        name = "success-service"
        address = "%s"
        port = "http"
				provider = "nomad"
      }
    }
  }
}`, dummyOkTcpAddr.Port, dummyOkTcpAddr.IP.String())

	failJobHCL := fmt.Sprintf(`
job "fail-service-test" {
  datacenters = ["dc1"]
  type = "service"

  group "web" {
    count = 1

    network {
      port "http" {
        static = %d
      }
    }

    task "dummy" {
			driver = "docker"

      config {
        image   = "alpine:latest"
        command = "sleep"
        args    = ["600"]
      }

      service {
        name = "fail-service"
        address = "%s"
        port = "http"
				provider = "nomad"
      }
    }
  }
}`, dummyNgTcpAddr.Port, dummyNgTcpAddr.IP.String())

	// Submit jobs to Nomad
	successJob, err := nomadClient.Jobs().ParseHCL(successJobHCL, true)
	require.Nil(err)
	_, _, err = nomadClient.Jobs().Register(successJob, nil)
	require.Nil(err)

	failJob, err := nomadClient.Jobs().ParseHCL(failJobHCL, true)
	require.Nil(err)
	_, _, err = nomadClient.Jobs().Register(failJob, nil)
	require.Nil(err)

	// Wait for jobs to start and services to be registered
	time.Sleep(5 * time.Second)

	err = waitForUrl(envoyAddr+"/success-service/", 200)
	require.Nil(err)

	err = waitForUrl(envoyAddr+"/fail-service/", 400) // expect: 400 from dummy server
	require.Nil(err)

	dummyOkServer.SetStatusCode(400)
	err = waitForUrl(envoyAddr+"/success-service/", 400) // expect: 400 from dummy server
	require.Nil(err)

	dummyOkServer.SetStatusCode(200)
	err = waitForUrl(envoyAddr+"/success-service/", 200)
	require.Nil(err)
}

func TestE2EBoth(t *testing.T) {

	require := require.New(t)

	{
		listen = "127.0.0.1:16000"
		httpListen = "127.0.0.1:16001"
		logLevel = "debug"
		logFormat = "text"
		enableConsul = true
		enableNomad = true
		consulAddr = ""
		nomadAddr = ""
	}
	var (
		envoyAddr      = "http://127.0.0.1:16101"
		envoyAdminAddr = "http://127.0.0.1:16102"
		consulApiAddr  = "127.0.0.1:16201"
		consulApi      = "http://127.0.0.1:16201"
		nomadApi       = "http://127.0.0.1:16301"
	)
	require.Nil(os.Setenv("CONSUL_HTTP_ADDR", consulApi))
	require.Nil(os.Setenv("NOMAD_ADDR", nomadApi))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		start(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// Create clients for service discovery backends early for cleanup
	consulConf := api.DefaultConfig()
	consulConf.Address = consulApiAddr
	consulClient, err := api.NewClient(consulConf)
	require.Nil(err)

	nomadConf := nomadapi.DefaultConfig()
	nomadConf.Address = nomadApi
	nomadClient, err := nomadapi.NewClient(nomadConf)
	require.Nil(err)

	defer func() {
		// Stop Nomad jobs before shutdown
		if nomadClient != nil {
			_, _, err := nomadClient.Jobs().Deregister("merged-service-test", true, nil)
			if err != nil {
				t.Logf("Failed to stop merged-service-test job: %v", err)
			}

			// Wait for jobs to be stopped
			time.Sleep(2 * time.Second)
		}

		// Deregister Consul service
		if consulClient != nil {
			consulClient.Agent().ServiceDeregister("merged-service-consul")
		}
	}()

	dummyOkListener, err := net.Listen("tcp", "127.0.0.1:0")
	dummyOkServer := &dummyServer{status: 200}
	require.Nil(err)
	go func() {
		err := dummyOkServer.StartDummyHttpServer(dummyOkListener)
		require.Nil(err)
	}()
	dummyNgListener, err := net.Listen("tcp", "127.0.0.1:0")
	dummyNgServer := &dummyServer{status: 400}
	require.Nil(err)
	go func() {
		err := dummyNgServer.StartDummyHttpServer(dummyNgListener)
		require.Nil(err)
	}()
	dummyConsulListener, err := net.Listen("tcp", "127.0.0.1:0")
	dummyConsulServer := &dummyServer{status: 200}
	require.Nil(err)
	go func() {
		err := dummyConsulServer.StartDummyHttpServer(dummyConsulListener)
		require.Nil(err)
	}()
	var (
		dummyOkTcpAddr     = dummyOkListener.Addr().(*net.TCPAddr)
		dummyConsulTcpAddr = dummyConsulListener.Addr().(*net.TCPAddr)
	)

	// wait for startup services
	err = waitForUrl(envoyAdminAddr+"/ready", 200)
	require.Nil(err)
	err = waitForUrl(consulApi+"/v1/status/leader", 200)
	require.Nil(err)
	err = waitForUrl(nomadApi+"/v1/status/leader", 200)
	require.Nil(err)
	t.Logf("test service is healthy")

	// Register the same service in both Consul and Nomad to test merging
	{
		// Register in Consul with health check
		consulService := &api.AgentServiceRegistration{
			ID:      "merged-service-consul",
			Name:    "merged-service",
			Address: dummyConsulTcpAddr.IP.String(),
			Port:    dummyConsulTcpAddr.Port,
			Check: &api.AgentServiceCheck{
				CheckID:  "merged_service_consul_check",
				Name:     "check",
				HTTP:     fmt.Sprintf("http://%s/", dummyConsulTcpAddr),
				Interval: "3s",
				Timeout:  "5s",
			},
		}
		err = consulClient.Agent().ServiceRegister(consulService)
		require.Nil(err)

		// Register in Nomad via job submission
		mergedJobHCL := fmt.Sprintf(`
job "merged-service-test" {
  datacenters = ["dc1"]
  type = "service"

  group "web" {
    count = 1

    network {
      port "http" {
        static = %d
      }
    }

    task "dummy" {
			driver = "docker"

      config {
        image   = "alpine:latest"
        command = "sleep"
        args    = ["600"]
      }

      service {
        name = "merged-service"
        address = "%s"
        port = "http"
				provider = "nomad"
      }
    }
  }
}`, dummyOkTcpAddr.Port, dummyOkTcpAddr.IP.String())

		mergedJob, err := nomadClient.Jobs().ParseHCL(mergedJobHCL, true)
		require.Nil(err)
		_, _, err = nomadClient.Jobs().Register(mergedJob, nil)
		require.Nil(err)

		// Wait for job to start and service to be registered
		time.Sleep(5 * time.Second)
	}

	// Test that both endpoints are available (service merging)
	err = waitForUrl(envoyAddr+"/merged-service/", 200)
	require.Nil(err)

	t.Logf("Both Consul and Nomad services are successfully merged and accessible")
}

func waitForUrl(url string, statusCode int) error {
	b := backoff.NewExponentialBackOff()
	b.MaxInterval = 3 * time.Second
	b.MaxElapsedTime = 30 * time.Second
	return backoff.Retry(func() error {
		res, err := http.Get(url)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != statusCode {
			fmt.Printf("invalid status code want:%d response:%d url:%s\n", statusCode, res.StatusCode, url)
			return fmt.Errorf("invalid status code want:%d response:%d url:%s", statusCode, res.StatusCode, url)
		}
		return nil
	}, b)
}

type dummyServer struct {
	sync.Mutex
	status int
}

func (d *dummyServer) StartDummyHttpServer(listener net.Listener) error {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		d.Lock()
		defer d.Unlock()
		writer.WriteHeader(d.status)
		writer.Write([]byte("ok"))
	})
	return http.Serve(listener, handler)
}
func (d *dummyServer) SetStatusCode(status int) {
	d.Lock()
	defer d.Unlock()
	d.status = status
}
