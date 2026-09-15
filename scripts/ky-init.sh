#!/usr/bin/env bash
set -euo pipefail

# ky-init.sh - Scaffold a new Busnes.app product from ky_server_base

if [ "$#" -lt 1 ]; then
    echo "Usage: $0 <app-name> [target-directory]"
    echo "Example: $0 kydrive-server ../kydrive-server"
    exit 1
fi

APP_NAME="$1"
TARGET_DIR="${2:-../$APP_NAME}"
BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "🚀 Initializing new Busnes.app product: ${APP_NAME}..."
echo "📂 Target Directory: ${TARGET_DIR}"

if [ -d "$TARGET_DIR" ] && [ "$(ls -A "$TARGET_DIR" 2>/dev/null)" ]; then
    echo "❌ Error: Target directory $TARGET_DIR already exists and is not empty."
    exit 1
fi

mkdir -p "$TARGET_DIR"

# Copy files excluding git, dist, node_modules, binaries, and local data
rsync -av --progress "$BASE_DIR/" "$TARGET_DIR/" \
    --exclude='.git' \
    --exclude='web/node_modules' \
    --exclude='web/dist' \
    --exclude='data' \
    --exclude='backups' \
    --exclude='ky_server_base' \
    --exclude='*.kycap'

cd "$TARGET_DIR"

MODULE_OLD="$(cd "$BASE_DIR" && go list -m)"
MODULE_NEW="github.com/Busness-app/${APP_NAME}"

echo "🔄 Updating Go module paths (${MODULE_OLD} -> ${MODULE_NEW})..."
find . -type f \( -name "*.go" -o -name "go.mod" -o -name "*.md" \) -exec sed -i "s|${MODULE_OLD}|${MODULE_NEW}|g" {} +

echo "🎨 Updating application branding & configuration..."
# Update package.json name
if [ -f "web/package.json" ]; then
    sed -i "s|\"name\": \"ky-server-base-web\"|\"name\": \"${APP_NAME}-web\"|g" web/package.json
fi

# Update HTML title
if [ -f "web/index.html" ]; then
    sed -i "s|<title>Busnes.app Base</title>|<title>${APP_NAME}</title>|g" web/index.html
fi

# Update Docker Compose container name
if [ -f "docker-compose.yml" ]; then
    sed -i "s|container_name: ky_server_base|container_name: ${APP_NAME}|g" docker-compose.yml
    sed -i "s|KY_APP_NAME=Busnes.app Base|KY_APP_NAME=${APP_NAME}|g" docker-compose.yml
fi

echo "📦 Running go mod tidy..."
go mod tidy

echo "✨ Successfully scaffolded ${APP_NAME} in ${TARGET_DIR}!"
echo ""
echo "To get started:"
echo "  cd ${TARGET_DIR}"
echo "  make all"
echo "  ./${APP_NAME}"
