#!/bin/sh
# Run only in a disposable Linux VM/container with systemd as PID 1.
set -eu
# Select the native build explicitly when the container userspace is emulated.
install -m 0755 "/src/dist/deitysight-linux-${DEITYSIGHT_TEST_ARCH:-amd64}" /usr/local/bin/deitysight
id deitysight >/dev/null 2>&1 || useradd --system --no-create-home --shell /sbin/nologin deitysight
install -d -m 0750 -o root -g deitysight /etc/deitysight
sed 's/REPLACE_WITH_DEPLOYMENT_SECRET/integration-only-token/; s/min_free_bytes: 1073741824/min_free_bytes: 1/' /src/configs/agent.example.yaml > /etc/deitysight/agent.yaml
chown root:deitysight /etc/deitysight/agent.yaml
chmod 0640 /etc/deitysight/agent.yaml
# Install atop 2.7.1 at /usr/bin/atop before this check.
/usr/bin/atop -V
install -m 0644 /src/deploy/deitysight.service /etc/systemd/system/deitysight.service
systemd-analyze verify /etc/systemd/system/deitysight.service
systemctl daemon-reload
systemctl start deitysight
systemctl --no-pager --full status deitysight
curl --retry 10 --retry-connrefused --retry-delay 1 --fail -s -H 'Authorization: Bearer integration-only-token' http://127.0.0.1:19100/v1/health
task_request_id="systemd-live-$(date +%s)"
curl --fail -s -H 'Authorization: Bearer integration-only-token' -H 'Content-Type: application/json' -d "{\"request_id\":\"$task_request_id\",\"window_seconds\":2,\"step_seconds\":1}" http://127.0.0.1:19100/v1/tasks
systemctl show deitysight -p CPUQuotaPerSecUSec -p MemoryMax -p MainPID -p ControlGroup
