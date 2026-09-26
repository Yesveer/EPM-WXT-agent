#!/bin/bash
# VSay Agent - Pre-removal Script

set -e

echo "Stopping VSay Agent..."

# Stop service before removal
systemctl stop vsay-agent 2>/dev/null || true
systemctl disable vsay-agent 2>/dev/null || true

exit 0
