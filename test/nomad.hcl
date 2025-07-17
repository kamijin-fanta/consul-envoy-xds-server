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

ports {
  http = 4646
  rpc  = 4647
  serf = 4648
}

consul {
  address = "127.0.0.1:16201"
  auto_advertise = true
}

log_level = "INFO"
enable_debug = false