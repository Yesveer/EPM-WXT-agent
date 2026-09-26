#!/bin/bash
# VSay Agent - Post-removal Script

set -e

# Reload systemd
systemctl daemon-reload 2>/dev/null || true

# On purge, remove config and logs
if [ "$1" = "purge" ] || [ "$1" = "0" ]; then
    rm -rf /etc/vsay 2>/dev/null || true
    rm -rf /var/log/vsay 2>/dev/null || true
    rm -f /etc/sudoers.d/vsay-* 2>/dev/null || true
    echo "VSay Agent configuration removed."
fi

exit 0
