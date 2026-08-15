#!/usr/bin/env bash
# homelab-tsdb install script.
#
# Tries to download a prebuilt binary from the latest GitHub Release first
# (fast, no Go toolchain needed on the target machine). Falls back to
# building from source if there's no matching release, the architecture
# isn't one we build for, or GitHub's API is unreachable/rate-limited.
#
# The one thing that does NOT fall back silently: a checksum mismatch. That
# can mean tampering, not just "no release available" - so it's a hard
# failure, not a soft one.
set -euo pipefail

REPO="BramVH98/homelab-tsdb"
REPO_URL="https://github.com/${REPO}.git"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="homelab-tsdb-logserver"
CONFIG_DIR="/etc/homelab-tsdb"
DATA_DIR="/var/lib/homelab-tsdb"
SERVICE_USER="homelab-tsdb"
BUILD_DIR="$(mktemp -d)"

bold() { printf "\033[1m%s\033[0m\n" "$1"; }
check() { printf "  \033[32m✓\033[0m %s\n" "$1"; }
cross() { printf "  \033[90m✗\033[0m %s (not detected)\n" "$1"; }

trap 'rm -rf "$BUILD_DIR"' EXIT

if [ "$(id -u)" -ne 0 ]; then
    echo "This installer needs root (it installs a systemd service and a system user)."
    echo "Try: curl ... | sudo bash"
    exit 1
fi

bold "homelab-tsdb installer"
echo ""

# --- 1. Figure out this machine's architecture, in release-asset terms ---
ARCH_SUFFIX=""
case "$(uname -m)" in
    x86_64)  ARCH_SUFFIX="linux-amd64" ;;
    aarch64|arm64) ARCH_SUFFIX="linux-arm64" ;;
    armv7l)  ARCH_SUFFIX="linux-armv7" ;;
    *)       ARCH_SUFFIX="" ;;  # covers armv6l (Pi Zero/1) and anything else - no prebuilt binary exists for these
esac

# --- 2. Try to fetch a prebuilt release binary ---
INSTALLED_FROM_RELEASE=false

if [ -n "$ARCH_SUFFIX" ]; then
    echo "Checking for a prebuilt release ($ARCH_SUFFIX)..."

    RELEASE_JSON="$BUILD_DIR/release.json"
    if curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" -o "$RELEASE_JSON" 2>/dev/null; then
        ASSET_NAME="${BINARY_NAME}-${ARCH_SUFFIX}"

        BINARY_URL="$(python3 -c "
import json, sys
try:
    with open('$RELEASE_JSON') as f:
        data = json.load(f)
    for asset in data.get('assets', []):
        if asset['name'] == '$ASSET_NAME':
            print(asset['browser_download_url'])
            sys.exit(0)
except Exception:
    pass
sys.exit(1)
" 2>/dev/null)" || BINARY_URL=""

        CHECKSUMS_URL="$(python3 -c "
import json, sys
try:
    with open('$RELEASE_JSON') as f:
        data = json.load(f)
    for asset in data.get('assets', []):
        if asset['name'] == 'checksums.txt':
            print(asset['browser_download_url'])
            sys.exit(0)
except Exception:
    pass
sys.exit(1)
" 2>/dev/null)" || CHECKSUMS_URL=""

        if [ -n "$BINARY_URL" ] && [ -n "$CHECKSUMS_URL" ]; then
            echo "Downloading $ASSET_NAME..."
            curl -fsSL "$BINARY_URL" -o "$BUILD_DIR/$ASSET_NAME"
            curl -fsSL "$CHECKSUMS_URL" -o "$BUILD_DIR/checksums.txt"

            echo "Verifying checksum..."
            if (cd "$BUILD_DIR" && sha256sum -c checksums.txt --ignore-missing) >/dev/null 2>&1; then
                install -m 755 "$BUILD_DIR/$ASSET_NAME" "$INSTALL_DIR/$BINARY_NAME"
                INSTALLED_FROM_RELEASE=true
                echo "Installed prebuilt binary (checksum verified)."
            else
                # Do NOT fall back silently here - a checksum mismatch can
                # mean a corrupted download or actual tampering, and quietly
                # working around it would hide exactly the thing that
                # matters most to catch.
                echo ""
                echo "ERROR: checksum verification failed for the downloaded binary."
                echo "This could mean a corrupted download or a tampered file - not proceeding."
                echo "Re-run the installer, and if this keeps happening, check the release page directly:"
                echo "  https://github.com/${REPO}/releases"
                exit 1
            fi
        else
            echo "No matching release asset found - will build from source instead."
        fi
    else
        echo "Could not reach GitHub's release API (rate-limited or offline) - will build from source instead."
    fi
else
    echo "No prebuilt binary for this architecture ($(uname -m)) - will build from source instead."
fi

# --- 3. Fall back to building from source if we didn't install a release ---
if [ "$INSTALLED_FROM_RELEASE" = false ]; then
    if ! command -v go >/dev/null 2>&1; then
        echo "Go not found - installing via apt..."
        apt-get update -qq
        apt-get install -y -qq golang-go
    fi

    echo "Fetching source..."
    git clone --quiet --depth 1 "$REPO_URL" "$BUILD_DIR/src"
    cd "$BUILD_DIR/src"

    echo "Building..."
    go build -o "$BUILD_DIR/$BINARY_NAME" ./cmd/logserver
    install -m 755 "$BUILD_DIR/$BINARY_NAME" "$INSTALL_DIR/$BINARY_NAME"
    echo "Built and installed from source."
fi

# --- 4. Detect what's on this machine ---
echo ""
bold "Detected:"

HAS_DOCKER=false
if command -v docker >/dev/null 2>&1; then
    check "Docker"
    HAS_DOCKER=true
else
    cross "Docker"
fi

HAS_APACHE=false
if systemctl is-active --quiet apache2 2>/dev/null || systemctl is-active --quiet httpd 2>/dev/null; then
    check "Apache"
    HAS_APACHE=true
else
    cross "Apache"
fi

HAS_NGINX=false
if systemctl is-active --quiet nginx 2>/dev/null; then
    check "Nginx"
    HAS_NGINX=true
else
    cross "Nginx"
fi

# --- 5. Set up user, directories, config ---
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

mkdir -p "$CONFIG_DIR" "$DATA_DIR"
chown -R "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR" "$CONFIG_DIR"

ADMIN_PASS="$(head -c 12 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 16)"

if [ ! -f "$CONFIG_DIR/logserver.conf" ]; then
    {
        echo "# homelab-tsdb logserver config - generated by installer"
        echo "addr = :8080"
        echo "auth-user = admin"
        echo "auth-pass = $ADMIN_PASS"
        echo "data = $DATA_DIR/data"
        echo "retention = 720h"
        echo "retention-check = 1h"
        echo "syslog-addr = :5514"
        echo "syslog-allow = 127.0.0.1/32"
    } > "$CONFIG_DIR/logserver.conf"
    chmod 600 "$CONFIG_DIR/logserver.conf"
    chown "$SERVICE_USER:$SERVICE_USER" "$CONFIG_DIR/logserver.conf"
fi

# --- 6. systemd unit ---
cat > /etc/systemd/system/homelab-tsdb.service << EOF
[Unit]
Description=homelab-tsdb log server
After=network.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
ExecStart=$INSTALL_DIR/$BINARY_NAME -config=$CONFIG_DIR/logserver.conf
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --quiet homelab-tsdb
systemctl restart homelab-tsdb
sleep 1

echo ""
if systemctl is-active --quiet homelab-tsdb; then
    bold "Dashboard available at :8080"
    echo "  user: admin"
    echo "  pass: $ADMIN_PASS"
    echo "  (saved in $CONFIG_DIR/logserver.conf)"
else
    echo "Service failed to start - check: journalctl -u homelab-tsdb -n 50"
    exit 1
fi

echo ""
echo "Next steps for what was detected:"
if [ "$HAS_APACHE" = true ]; then
    echo "  Apache: pipe access logs in with the 'logger' command, e.g.:"
    echo "    CustomLog \"|/usr/bin/logger -n 127.0.0.1 -P 5514 -d -t apache\" combined"
fi
if [ "$HAS_NGINX" = true ]; then
    echo "  Nginx supports syslog natively - add to your config:"
    echo "    access_log syslog:server=127.0.0.1:5514,tag=nginx combined;"
fi

if [ "$HAS_DOCKER" = true ]; then
    echo "  Docker: configuring syslog as the default log driver for new containers..."
    python3 << 'PYEOF'
import json, os, time, shutil

path = "/etc/docker/daemon.json"
existing = {}

if os.path.exists(path):
    backup = f"{path}.bak-{time.time_ns()}"
    shutil.copy2(path, backup)
    print(f"    backed up existing config to {backup}")
    try:
        with open(path) as f:
            existing = json.load(f)
    except (json.JSONDecodeError, ValueError):
        print(f"    WARNING: existing {path} wasn't valid JSON - starting fresh (original is backed up above)")
        existing = {}

existing["log-driver"] = "syslog"
existing["log-opts"] = {
    "syslog-address": "udp://127.0.0.1:5514",
    "tag": "{{.Name}}",
}

os.makedirs(os.path.dirname(path), exist_ok=True)
with open(path, "w") as f:
    json.dump(existing, f, indent=2)
    f.write("\n")
print(f"    updated {path}")
PYEOF
    echo "  NOTE: this only affects NEW containers. Already-running containers"
    echo "  need to be recreated (docker compose up -d --force-recreate, or"
    echo "  docker stop/rm + run again) to pick up the new default log driver."
    if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet docker 2>/dev/null; then
        systemctl restart docker
        echo "  Docker restarted to apply the change."
    fi
fi

if [ "$HAS_APACHE" = true ] || [ "$HAS_NGINX" = true ]; then
    echo ""
    echo "  Apache/Nginx config isn't edited automatically - their config"
    echo "  layout varies too much between systems to safely guess at, so"
    echo "  the one-liner above is copy-pasteable into your existing config."
fi