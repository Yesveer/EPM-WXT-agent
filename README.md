# Vsay Agent

Lightweight Go agent that runs on Linux machines to enable secure remote terminal access via **gRPC**.

## 🔌 Communication Protocol

**gRPC** is used for all communication between agent and backend:

- ✅ **High Performance**: Binary protocol, faster than REST
- ✅ **Bidirectional Streaming**: Real-time command execution
- ✅ **Type Safety**: Protocol buffers ensure type safety
- ✅ **Efficient**: Lower overhead than HTTP/JSON

### gRPC Services

1. **Register**: Agent registration with backend
2. **Heartbeat**: Periodic health checks
3. **Stream**: Bidirectional streaming for commands and output

## 🚀 Installation

[Previous installation instructions remain the same...]

## 📋 Prerequisites

- Linux system (Ubuntu, Debian, RHEL, CentOS, Fedora, Arch, Alpine)
- Root or sudo access
- Network connectivity to backend server (gRPC port, default: 50051)

## 🔧 Configuration

After installation, configure the agent. **The daemon will automatically start after configuration:**

```bash
sudo vsay-agent configure \
  --token "YOUR_AGENT_TOKEN" \
  --tenant "tenant-id" \
  --org "organization-id" \
  --project "project-id" \
  --user "username" \
  --linux-user "username" \
  --host "http://192.168.1.5:8080" \
  --allow-sudo
```

**Note:** The `--host` parameter accepts HTTP URL, but agent automatically extracts gRPC endpoint:
- `http://192.168.1.5:8080` → gRPC: `192.168.1.5:50051`
- `https://api.vsay.io` → gRPC: `api.vsay.io:50051`

## 🛠️ Building from Source

### Generate Protobuf Code

```bash
# Install protobuf tools first
make proto-install

# Generate Go code from proto files
make proto
```

### Build Agent

```bash
make build
```

## 📡 gRPC Communication Flow

```
Agent Startup
    ↓
Connect to Backend (gRPC)
    ↓
Register Agent
    ↓
Start Bidirectional Stream
    ↓
┌─────────────────────────┐
│  gRPC Stream Loop       │
│                         │
│  ← Receive Commands     │
│  → Send Output          │
│  ← Receive Terminal Input│
│  → Send Terminal Output │
│  ← Config Updates       │
│  → Heartbeats           │
└─────────────────────────┘
```

## 🔐 Security

- **Token Authentication**: All gRPC calls authenticated with token
- **TLS Support**: Can be enabled for encrypted communication
- **Linux User Execution**: Commands execute as specified Linux user (not root)

## 📚 Protocol Buffers

Proto files are in `proto/` directory:
- `proto/common/types.proto` - Common types
- `proto/agent/agent.proto` - Agent service definitions

Generate Go code:
```bash
make proto
```

## 📄 License

MIT
