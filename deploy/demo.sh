#!/bin/sh
# Start the whole stack locally and drive it with simulated terminals.
#
# Everything binds to loopback and the dataplane stays in dry-run, so this
# changes nothing on the host.
set -eu

cd "$(dirname "$0")/.."
RUN=var/run
mkdir -p "$RUN"

stop() {
    for name in noc oss basestationd; do
        if [ -f "$RUN/$name.pid" ]; then
            kill "$(cat "$RUN/$name.pid")" 2>/dev/null || true
            rm -f "$RUN/$name.pid"
            echo "stopped $name"
        fi
    done
}

if [ "${1:-start}" = "stop" ]; then
    stop
    exit 0
fi

trap stop EXIT INT TERM

if [ ! -f var/config.json ]; then
    echo "creating var/config.json from the example"
    mkdir -p var
    sed 's/"admin_token": ""/"admin_token": "dev-admin-token"/; s/"tick_seconds": 15/"tick_seconds": 5/; s/"solver_timeout_seconds": 5/"solver_timeout_seconds": 3/' \
        deploy/config.example.json > var/config.json
fi

if [ ! -f var/inventory.json ]; then
    ./bin/basestationd -config var/config.json seed
fi
if [ ! -f var/oss.json ]; then
    java -jar oss/build/bswisp-oss.jar seed --store var/oss.json
fi

echo "starting control plane..."
./bin/basestationd -config var/config.json run > var/basestationd.log 2>&1 &
echo $! > "$RUN/basestationd.pid"

i=0
while [ $i -lt 20 ]; do
    curl -fsS -m 1 http://127.0.0.1:8080/healthz >/dev/null 2>&1 && break
    i=$((i + 1))
done

echo "starting NOC console..."
CONTROL_PLANE_TOKEN=dev-admin-token Noc__Listen=http://127.0.0.1:8070 \
    dotnet run --project noc/BswispNoc.csproj --no-build > var/noc.log 2>&1 &
echo $! > "$RUN/noc.pid"

echo
echo "  control plane  http://127.0.0.1:8080  (admin token: dev-admin-token)"
echo "  NOC console    http://127.0.0.1:8070"
echo
echo "driving 5 simulated terminals; Ctrl-C to stop"
echo
./bin/termsim -demand 80 -v
