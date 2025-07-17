job "test-services" {
  datacenters = ["dc1"]
  type = "service"

  # Nomad service provider group
  group "nomad-services" {
    count = 1

    network {
      port "web" {
        static = 8080
      }
      port "api" {
        static = 8081
      }
    }


    task "web-server" {
      driver = "docker"

      config {
        image = "alpine:3.18"
        command = "sleep"
        args = ["3600"]
      }

      resources {
        cpu    = 100
        memory = 64
      }

      # Register with Nomad service registry
      service {
        name = "test-web-service"
        address = "127.0.0.1"
        port = "web"
        provider = "nomad"


        tags = [
          "nomad-provider",
          "test-service",
          "web"
        ]
      }
    }

    task "api-server" {
      driver = "docker"

      config {
        image = "alpine:3.18"
        command = "sleep"
        args = ["3600"]
      }

      resources {
        cpu    = 100
        memory = 64
      }

      # Register with Nomad service registry
      service {
        name = "test-api-service"
        address = "127.0.0.1"
        port = "api"
        provider = "nomad"


        tags = [
          "nomad-provider",
          "test-service",
          "api"
        ]
      }
    }
  }

  # Consul service provider group
  group "consul-services" {
    count = 1

    network {
      port "web-consul" {
        static = 8082
      }
      port "consul-only" {
        static = 8083
      }
    }


    task "web-server-consul" {
      driver = "docker"

      config {
        image = "alpine:3.18"
        command = "sleep"
        args = ["3600"]
      }

      resources {
        cpu    = 100
        memory = 64
      }

      # Register with Consul service registry 
      service {
        name = "test-web-service"
        address = "127.0.0.1"
        port = "web-consul"
        provider = "consul"


        tags = [
          "consul-provider",
          "test-service",
          "web"
        ]
      }
    }

    task "consul-only-service" {
      driver = "docker"

      config {
        image = "alpine:3.18"
        command = "sleep"
        args = ["3600"]
      }

      resources {
        cpu    = 100
        memory = 64
      }

      # Register with Consul service registry
      service {
        name = "test-consul-service"
        address = "127.0.0.1"
        port = "consul-only"
        provider = "consul"


        tags = [
          "consul-provider",
          "test-service",
          "consul"
        ]
      }
    }
  }
}