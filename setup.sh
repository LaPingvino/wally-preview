#!/bin/bash
set -e

echo "============================================="
echo "   Wally Preview Installer & Setup Helper"
echo "============================================="
echo ""

# 1. Compile if running in the source repository and binary doesn't exist
if [ ! -f wally-preview ] && [ -f main.go ]; then
  echo "Wally-preview binary not found. Compiling from source..."
  if ! command -v go &> /dev/null; then
    echo "Error: Go is not installed and is required to compile wally-preview."
    exit 1
  fi
  go build -o wally-preview .
  echo "Compilation successful."
fi

# 2. Install the binary to /usr/local/bin/
if [ -f wally-preview ]; then
  echo "Installing binary to /usr/local/bin/wally-preview..."
  sudo cp wally-preview /usr/local/bin/wally-preview
  sudo chmod 755 /usr/local/bin/wally-preview
else
  if [ ! -f /usr/local/bin/wally-preview ] && [ ! -f /usr/bin/wally-preview ]; then
    echo "Error: Could not find wally-preview binary to install."
    exit 1
  fi
fi

# 3. Install the systemd service
if [ -f wally-preview.service ]; then
  echo "Installing systemd service file..."
  sudo cp wally-preview.service /etc/systemd/system/wally-preview.service
  sudo chmod 644 /etc/systemd/system/wally-preview.service
fi

# 4. Gather configuration details
echo ""
echo "--- Configuration ---"
read -p "Enter your local homeserver API URL [http://127.0.0.1:8008]: " HOMESERVER_URL
HOMESERVER_URL=${HOMESERVER_URL:-http://127.0.0.1:8008}

# Generate a secure random password
PASSWORD=$(python3 -c 'import secrets; print(secrets.token_urlsafe(24))' 2>/dev/null || openssl rand -base64 18 2>/dev/null || echo "wallyPassword123!")

echo ""
echo "--- Account Creation ---"
echo "Please create a DEDICATED, low-privilege account for wally-preview."
echo "If you use Conduwuit/Continuwuity, go to your server admin room and run:"
echo ""
echo "    !admin users create-user wally-preview $PASSWORD"
echo ""
echo "If you use Synapse, run this on your server terminal:"
echo "    register_new_matrix_user -c /etc/matrix-synapse/homeserver.yaml -u wally-preview -p $PASSWORD --no-admin"
echo ""
read -p "Once the user has been created, press [Enter] to continue and log in..."

# 5. Log in to retrieve access token
echo ""
echo "Logging in to retrieve the access token from $HOMESERVER_URL..."
RESPONSE=$(curl -sS -X POST -H "Content-Type: application/json" \
  -d "{\"type\":\"m.login.password\", \"identifier\":{\"type\":\"m.id.user\", \"user\":\"wally-preview\"}, \"password\":\"$PASSWORD\"}" \
  "$HOMESERVER_URL/_matrix/client/v3/login" || echo "")

if [ -z "$RESPONSE" ] || echo "$RESPONSE" | grep -q "errcode"; then
  echo "Error logging in: $RESPONSE"
  echo "Make sure the user was successfully created and the homeserver is running at $HOMESERVER_URL."
  exit 1
fi

TOKEN=$(echo "$RESPONSE" | python3 -c "import sys, json; data=json.load(sys.stdin); print(data.get('access_token', ''))" 2>/dev/null || \
        echo "$RESPONSE" | grep -oP '"access_token":"\K[^"]+')

if [ -z "$TOKEN" ]; then
  echo "Failed to extract access token from response: $RESPONSE"
  exit 1
fi

echo "Successfully logged in and obtained access token."
echo ""

# 6. Create the environment file
echo "Creating /etc/wally-preview.env..."
cat <<EOF | sudo tee /etc/wally-preview.env > /dev/null
# wally-preview configuration (all via environment variables).

# Where the shim listens. Bind to localhost — Caddy/nginx is the only ingress.
WALLY_PREVIEW_LISTEN=127.0.0.1:8088

# The local homeserver, used only for whoami (authz) and media upload (mint mxc).
WALLY_PREVIEW_HOMESERVER=$HOMESERVER_URL

# Access token of a DEDICATED, low-privilege Matrix account used to upload
# preview images so they get a local mxc:// URI.
WALLY_PREVIEW_UPLOAD_TOKEN=$TOKEN

# Configuration options
WALLY_PREVIEW_MAX_HTML_BYTES=262144
WALLY_PREVIEW_MAX_IMAGE_BYTES=5242880
WALLY_PREVIEW_CACHE_TTL=1h
WALLY_PREVIEW_NEGATIVE_TTL=5m
WALLY_PREVIEW_MAX_CONCURRENCY=8
WALLY_PREVIEW_REQUEST_TIMEOUT=10s
WALLY_PREVIEW_USER_AGENT="wally-preview/0.1 (+https://github.com/LaPingvino/wally-preview)"
EOF

sudo chmod 600 /etc/wally-preview.env
sudo chown root:root /etc/wally-preview.env
echo "/etc/wally-preview.env written securely."
echo ""

# 7. Start and Enable Service
echo "Enabling and starting wally-preview service..."
sudo systemctl daemon-reload
sudo systemctl enable --now wally-preview
echo ""

# 8. Verification
echo "--- Verification ---"
echo "Checking service status..."
sudo systemctl status wally-preview.service --no-pager
echo ""

echo "Checking healthz endpoint..."
curl -fsS http://127.0.0.1:8088/healthz && echo " -> OK"
echo ""

echo "--- Reverse Proxy Setup ---"
echo "Now route the preview endpoints to the wally-preview shim in your web server."
echo ""
echo "For Caddy:"
echo "----------------------------------------"
echo "    @preview path /_matrix/media/*/preview_url /_matrix/client/*/media/preview_url"
echo "    handle @preview {"
echo "        reverse_proxy localhost:8088"
echo "    }"
echo "----------------------------------------"
echo ""
echo "For Nginx:"
echo "----------------------------------------"
echo "    location ~ ^/_matrix/(media|client)/.*/(media/)?preview_url$ {"
echo "        proxy_pass http://127.0.0.1:8088;"
echo "        proxy_set_header Host \$host;"
echo "    }"
echo "----------------------------------------"
echo ""
echo "Remember to disable your homeserver's OWN url previews so it never fetches untrusted content."
echo "Wally preview setup complete!"
