#!/bin/bash
# VSay Agent - Pre-removal Script

set -e

echo "Stopping VSay Agent..."

# Stop service before removal
systemctl stop wxt-agent 2>/dev/null || true
systemctl disable wxt-agent 2>/dev/null || true

exit 0
