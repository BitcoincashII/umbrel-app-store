#!/bin/bash
set -e

# Generate config from template with environment variables
envsubst < /etc/forge/config.template.yaml > /etc/forge/config.yaml

# Start stratum server
exec /usr/local/bin/stratum -config /etc/forge/config.yaml
