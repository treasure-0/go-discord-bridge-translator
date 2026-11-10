#!/usr/bin/env bash
# Installs or updates the bot on an Ubuntu server as a systemd service.
# Run it again any time to pull the latest code and restart.
#
#   curl -fsSL https://raw.githubusercontent.com/treasure-0/go-discord-bridge-translator/main/deploy/install.sh | sudo bash
set -euo pipefail

REPO="${REPO:-https://github.com/treasure-0/go-discord-bridge-translator.git}"
BRANCH="${BRANCH:-main}"
GO_VERSION="${GO_VERSION:-1.25.4}"
APP_DIR=/opt/discord-translator
ENV_FILE=/etc/discord-translator.env
SERVICE=discord-translator

if [[ $EUID -ne 0 ]]; then
  echo "Run as root: sudo bash install.sh" >&2
  exit 1
fi

echo "==> Packages"
apt-get update -qq
apt-get install -y -qq git ca-certificates curl >/dev/null

# Small Always Free VMs have 1 GB RAM; compiling the Gemini SDK needs a bit more.
if ! swapon --show | grep -q .; then
  echo "==> Adding 2 GB swap"
  fallocate -l 2G /swapfile
  chmod 600 /swapfile
  mkswap /swapfile >/dev/null
  swapon /swapfile
  grep -q '^/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi

echo "==> Go ${GO_VERSION}"
case "$(uname -m)" in
  x86_64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "Unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION} "; then
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tgz
  rm /tmp/go.tgz
fi
export PATH=/usr/local/go/bin:$PATH
export GOCACHE=/root/.cache/go-build GOPATH=/root/go

echo "==> Code"
if [[ -d "$APP_DIR/src/.git" ]]; then
  git -C "$APP_DIR/src" fetch -q origin "$BRANCH"
  git -C "$APP_DIR/src" reset -q --hard "origin/$BRANCH"
else
  mkdir -p "$APP_DIR"
  git clone -q --branch "$BRANCH" "$REPO" "$APP_DIR/src"
fi

echo "==> Build"
mkdir -p "$APP_DIR/bin"
(cd "$APP_DIR/src" && go build -trimpath -ldflags="-s -w" -o "$APP_DIR/bin/$SERVICE.new" .)
mv "$APP_DIR/bin/$SERVICE.new" "$APP_DIR/bin/$SERVICE"

echo "==> Service"
id -u translator >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin translator
if [[ ! -f "$ENV_FILE" ]]; then
  install -m 600 -o root -g root "$APP_DIR/src/.env.example" "$ENV_FILE"
  FIRST_RUN=1
fi
install -m 644 "$APP_DIR/src/deploy/$SERVICE.service" "/etc/systemd/system/$SERVICE.service"
systemctl daemon-reload
systemctl enable -q "$SERVICE"

if [[ "${FIRST_RUN:-0}" == 1 ]]; then
  echo
  echo "Installed. Now put your tokens into $ENV_FILE:"
  echo "  sudo nano $ENV_FILE"
  echo "then start the bot:"
  echo "  sudo systemctl start $SERVICE && journalctl -u $SERVICE -f"
else
  systemctl restart "$SERVICE"
  sleep 2
  systemctl --no-pager --lines=5 status "$SERVICE" || true
fi
