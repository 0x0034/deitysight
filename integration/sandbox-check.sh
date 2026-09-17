#!/bin/sh
set -eu
# Run after systemd-check.sh, inside the same disposable container.
install -m 0755 /src/dist/sandboxprobe /usr/local/bin/deitysight-sandboxprobe
sed -e 's@ExecStart=.*@ExecStart=/usr/local/bin/deitysight-sandboxprobe@' \
    -e 's/Type=simple/Type=oneshot/' -e 's/Restart=on-failure/Restart=no/' \
    /src/deploy/deitysight.service > /etc/systemd/system/deitysight-sandboxprobe.service
systemctl daemon-reload
systemd-run --unit deitysight-fixture --uid nobody /usr/bin/sleep 120
systemctl start deitysight-sandboxprobe
journalctl -u deitysight-sandboxprobe --no-pager -n 30
systemctl stop deitysight-fixture
