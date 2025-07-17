#!/bin/bash

set -e

NOMAD_ADDR="http://localhost:16301"
CONSUL_HTTP_ADDR="http://localhost:16201"

echo "Registering test services to Nomad cluster..."
echo "Nomad Address: $NOMAD_ADDR"
echo "Consul Address: $CONSUL_HTTP_ADDR"

# Wait for Nomad to be ready
echo "Waiting for Nomad to be ready..."
for i in {1..30}; do
    if curl -s "$NOMAD_ADDR/v1/status/leader" > /dev/null 2>&1; then
        echo "Nomad is ready!"
        break
    fi
    if [ $i -eq 30 ]; then
        echo "Timeout waiting for Nomad to be ready"
        exit 1
    fi
    echo "Waiting... ($i/30)"
    sleep 2
done

# Wait for Consul to be ready
echo "Waiting for Consul to be ready..."
for i in {1..30}; do
    if curl -s "$CONSUL_HTTP_ADDR/v1/status/leader" > /dev/null 2>&1; then
        echo "Consul is ready!"
        break
    fi
    if [ $i -eq 30 ]; then
        echo "Timeout waiting for Consul to be ready"
        exit 1
    fi
    echo "Waiting... ($i/30)"
    sleep 2
done

# Register job to Nomad
echo "Submitting test-services job to Nomad..."
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NOMAD_ADDR="$NOMAD_ADDR" nomad job run "$SCRIPT_DIR/test-services.nomad.hcl"

echo "Job submitted successfully!"

# Wait a bit for services to start
echo "Waiting for services to start..."
sleep 10

# Check job status
echo "Job status:"
NOMAD_ADDR="$NOMAD_ADDR" nomad job status test-services

echo ""
echo "Service information:"
echo ""

# Check Nomad services
echo "=== Nomad Services ==="
curl -s "$NOMAD_ADDR/v1/services" | jq -r '.[] | .ServiceName' | sort | uniq | while read service; do
    echo "Service: $service"
    curl -s "$NOMAD_ADDR/v1/service/$service" | jq -r '.[] | "  - " + .Address + ":" + (.Port | tostring) + " (" + .ID + ")"'
    echo ""
done

# Check Consul services
echo "=== Consul Services ==="
curl -s "$CONSUL_HTTP_ADDR/v1/catalog/services" | jq -r 'keys[]' | grep "test-" | while read service; do
    echo "Service: $service"
    curl -s "$CONSUL_HTTP_ADDR/v1/catalog/service/$service" | jq -r '.[] | "  - " + .ServiceAddress + ":" + (.ServicePort | tostring) + " (" + .ServiceID + ")"'
    echo ""
done

echo ""
echo "Service registration completed!"
echo ""
echo "To check xDS server debug page, run your xDS server with both providers enabled:"
echo "  ./consul-envoy-xds-server -enable-nomad=true -consul-addr=$CONSUL_HTTP_ADDR -nomad-addr=$NOMAD_ADDR"
echo ""
echo "Then visit the debug page at http://localhost:15001/debug to see merged endpoints."