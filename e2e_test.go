package main

import (
	"fmt"
	"github.com/cenkalti/backoff/v4"
	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/require"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestE2E(t *testing.T) {
	//assert := assert.New(t)
	require := require.New(t)

	{
		listen = "127.0.0.1:16000"
		httpListen = "127.0.0.1:16001"
		//logLevel = "warning"
		logLevel = "debug"
		logFormat = "text"
	}
	var (
		envoyAddr      = "http://127.0.0.1:16101"
		envoyAdminAddr = "http://127.0.0.1:16102"
		consulApiAddr  = "127.0.0.1:16201"
		consulApi      = "http://127.0.0.1:16201"
	)
	require.Nil(os.Setenv("CONSUL_HTTP_ADDR", consulApi))

	go func() {
		start()
		t.Fatal("server exits")
	}()

	cmd := exec.Command("docker-compose", "--file=./test/docker-compose.yaml", "--project-name=consul-envoy-xds-server-test", "up", "-d")
	err := cmd.Run()
	require.Nil(err)
	defer func() {
		cmd := exec.Command("docker-compose", "--file", "./test/docker-compose.yaml", "--project-name=consul-envoy-xds-server-test", "down")
		err := cmd.Run()
		require.Nil(err)
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
				CheckID:  "success_service_check",
				Name:     "check",
				HTTP:     dummyOkAddr,
				Interval: "3s",
				Timeout:  "5s",
			},
		}
		err = consulClient.Agent().ServiceRegister(successService)
		require.Nil(err)
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

func waitForUrl(url string, statusCode int) error {
	return backoff.Retry(func() error {
		res, err := http.Get(url)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != statusCode {
			fmt.Printf("invalid status code want:%d reponse:%d url:%s\n", statusCode, res.StatusCode, url)
			return fmt.Errorf("invalid status code want:%d reponse:%d url:%s", statusCode, res.StatusCode, url)
		}
		return nil
	}, backoff.NewExponentialBackOff())
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
