#!/bin/sh
set -e

if [ -n "$FLY_PRIVATE_IP" ]; then
  # Bind on all interfaces (dual-stack) so both the Fly health check
  # (127.0.0.1) and cross-machine gRPC (6PN IPv6) can reach port 7233.
  # BROADCAST_ADDRESS tells the membership layer which IP to advertise
  # (reachable as temporal.process.<app>.internal from other machines).
  export BIND_ON_IP="::"
  export TEMPORAL_BROADCAST_ADDRESS="$FLY_PRIVATE_IP"
fi

# Pass "autosetup" so the entrypoint creates the temporal databases and
# applies schema migrations before starting the server.
exec /etc/temporal/entrypoint.sh autosetup
