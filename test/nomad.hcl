datacenter = "dc1"
data_dir = "/tmp/nomad-data"

bind_addr = "0.0.0.0"

server {
  enabled = true
  bootstrap_expect = 1
}

client {
  enabled = true
}

plugin "raw_exec" {
  config {
    enabled = true
  }
}

ports {
  http = 4646
  rpc  = 4647
  serf = 4648
}

consul {
  # Disable consul integration to avoid conflicts
  auto_advertise = false
}

log_level = "INFO"
enable_debug = false