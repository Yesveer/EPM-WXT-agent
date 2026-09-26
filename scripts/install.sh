#!/bin/bash

# Quick install script (called from main install.sh)
# This is a helper script for the main installer

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Run main installer
"$PROJECT_ROOT/install.sh" "$@"
