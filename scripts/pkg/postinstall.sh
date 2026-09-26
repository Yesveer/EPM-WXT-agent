#!/bin/bash
# VSay Agent - Post Installation Script

set -e

echo ""
echo "╔════════════════════════════════════════════════════════════╗"
echo "║           VSay Agent installed successfully!               ║"
echo "╚════════════════════════════════════════════════════════════╝"
echo ""

# Create directories
mkdir -p /etc/vsay
mkdir -p /var/log/vsay
chmod 755 /etc/vsay
chmod 755 /var/log/vsay

# Ensure binary is executable
chmod 755 /usr/local/bin/vsay-agent 2>/dev/null || true

# Reload systemd
systemctl daemon-reload 2>/dev/null || true

# Enable service if config exists
if [ -f /etc/vsay/agent.yaml ]; then
    systemctl enable vsay-agent 2>/dev/null || true
    echo "Configuration found. Run 'sudo systemctl start vsay-agent' to start."
else
    echo "Configure the agent with:"
    echo ""
    echo "  sudo vsay-agent configure \\"
    echo "    --token <token> \\"
    echo "    --tenant <tenant> \\"
    echo "    --org <org> \\"
    echo "    --project <project> \\"
    echo "    --user <email> \\"
    echo "    --linux-user <username> \\"
    echo "    --host <server-url> \\"
    echo "    --allow-sudo"
    echo ""
    echo "Then start with: sudo systemctl start vsay-agent"
fi
echo ""

exit 0
