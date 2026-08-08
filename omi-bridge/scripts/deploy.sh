#!/bin/bash
set -euo pipefail

# ─────────────────────────────────────────────────────────────────────────────
# Deploy omi-bridge to Google Cloud Run (source-based build).
#
#   1. Reads config from omi-bridge/.env
#   2. Creates a dedicated, minimal-privilege service account
#   3. Deploys with --allow-unauthenticated so Spark can reach it; the MCP
#      tools are protected by OAuth 2.1 / JWT + optional OMI_BRIDGE_PASSPHRASE.
# ─────────────────────────────────────────────────────────────────────────────

SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BRIDGE_DIR="$(dirname "$SCRIPTS_DIR")"
PROJECT_ROOT="$(dirname "$BRIDGE_DIR")"
ENV_FILE="$BRIDGE_DIR/.env"

DEFAULT_REGION="us-central1"
DEFAULT_SERVICE_NAME="omi-mcp-bridge"

if [ -f "$ENV_FILE" ]; then
    echo "Loading deployment config from omi-bridge/.env..."
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
else
    echo "No omi-bridge/.env found — creating template..."
    RANDOM_JWT_KEY=$(openssl rand -hex 32 2>/dev/null || od -vN 32 -An -tx1 /dev/urandom | tr -d ' \n' | head -c 64)
    cat <<EOF > "$ENV_FILE"
GCP_PROJECT=
GCP_REGION=$DEFAULT_REGION
SERVICE_NAME=$DEFAULT_SERVICE_NAME
JWT_SIGNING_KEY=$RANDOM_JWT_KEY
OMI_MCP_API_KEY=
OMI_BRIDGE_PASSPHRASE=
EOF
    echo "Created omi-bridge/.env — set GCP_PROJECT and OMI_MCP_API_KEY, then re-run."
    exit 1
fi

: "${GCP_PROJECT:?Set GCP_PROJECT in omi-bridge/.env}"
: "${OMI_MCP_API_KEY:?Set OMI_MCP_API_KEY in omi-bridge/.env}"

GCP_REGION="${GCP_REGION:-$DEFAULT_REGION}"
SERVICE_NAME="${SERVICE_NAME:-$DEFAULT_SERVICE_NAME}"
OMI_API_BASE_URL="${OMI_API_BASE_URL:-https://api.omi.me}"
OMI_BRIDGE_PASSPHRASE="${OMI_BRIDGE_PASSPHRASE:-}"

if [ -z "${JWT_SIGNING_KEY:-}" ]; then
    echo "JWT_SIGNING_KEY not set; generating a random key..."
    JWT_SIGNING_KEY=$(openssl rand -hex 32 2>/dev/null || od -vN 32 -An -tx1 /dev/urandom | tr -d ' \n' | head -c 64)
fi

echo "========================================="
echo " Deploying $SERVICE_NAME to Cloud Run"
echo "  Project: $GCP_PROJECT"
echo "  Region:  $GCP_REGION"
echo "========================================="

gcloud config set project "$GCP_PROJECT" --quiet

SA_NAME="omi-mcp-bridge-runner"
SA_EMAIL="$SA_NAME@$GCP_PROJECT.iam.gserviceaccount.com"
if ! gcloud iam service-accounts describe "$SA_EMAIL" &>/dev/null; then
    echo "Creating service account $SA_EMAIL..."
    gcloud iam service-accounts create "$SA_NAME" \
        --display-name="Omi MCP Bridge runner" --quiet
fi

# Build using Cloud Build with omi-bridge/Dockerfile, then deploy container image.
IMAGE="gcr.io/$GCP_PROJECT/$SERVICE_NAME:$(date +%s)"

echo "Building container image $IMAGE via Cloud Build..."
gcloud builds submit "$PROJECT_ROOT" \
    --config "$BRIDGE_DIR/cloudbuild.yaml" \
    --substitutions="_IMAGE=$IMAGE" \
    --quiet

echo "Deploying image $IMAGE to Cloud Run..."
gcloud run deploy "$SERVICE_NAME" \
    --image "$IMAGE" \
    --region "$GCP_REGION" \
    --memory "256Mi" \
    --cpu "1" \
    --port "8080" \
    --timeout "3600" \
    --service-account "$SA_EMAIL" \
    --allow-unauthenticated \
    --session-affinity \
    --set-env-vars "JWT_SIGNING_KEY=$JWT_SIGNING_KEY,OMI_MCP_API_KEY=$OMI_MCP_API_KEY,OMI_API_BASE_URL=$OMI_API_BASE_URL,OMI_BRIDGE_PASSPHRASE=$OMI_BRIDGE_PASSPHRASE"

URL=$(gcloud run services describe "$SERVICE_NAME" --region "$GCP_REGION" --format="value(status.url)")
echo ""
echo "========================================="
echo " Deployed!"
echo "  Service URL:  $URL"
echo "  MCP endpoint: $URL/mcp   (or paste the bare $URL into Spark)"
echo "  PRM:          $URL/.well-known/oauth-protected-resource"
echo "========================================="
echo "Paste the Service URL into Gemini Spark → Connected Apps → custom app."
