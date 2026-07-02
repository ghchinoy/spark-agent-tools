#!/bin/bash
set -euo pipefail

# ─────────────────────────────────────────────────────────────────────────────
# Deploy hello-spark-mcp to Google Cloud Run (source-based build).
#
#   1. Reads config from .env (creating one with a random JWT key if missing).
#   2. Creates a dedicated, minimal-privilege service account (no extra roles —
#      this demo has no Firestore/Vertex dependencies).
#   3. Deploys with --allow-unauthenticated so Spark can reach it; the MCP tools
#      are still protected by our own OAuth 2.1 / JWT layer inside the app.
# ─────────────────────────────────────────────────────────────────────────────

SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPTS_DIR")"
ENV_FILE="$PROJECT_ROOT/.env"

DEFAULT_REGION="us-central1"
DEFAULT_SERVICE_NAME="hello-spark-mcp"

if [ -f "$ENV_FILE" ]; then
    echo "Loading deployment config from .env..."
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
else
    echo "No .env found — creating one with a generated JWT key..."
    RANDOM_JWT_KEY=$(openssl rand -hex 32 2>/dev/null || od -vN 32 -An -tx1 /dev/urandom | tr -d ' \n' | head -c 64)
    cat <<EOF > "$ENV_FILE"
GCP_PROJECT=
GCP_REGION=$DEFAULT_REGION
SERVICE_NAME=$DEFAULT_SERVICE_NAME
JWT_SIGNING_KEY=$RANDOM_JWT_KEY
EOF
    echo "Created $ENV_FILE — set GCP_PROJECT and re-run."
    exit 1
fi

: "${GCP_PROJECT:?Set GCP_PROJECT in .env}"
GCP_REGION="${GCP_REGION:-$DEFAULT_REGION}"
SERVICE_NAME="${SERVICE_NAME:-$DEFAULT_SERVICE_NAME}"
if [ -z "${JWT_SIGNING_KEY:-}" ]; then
    echo "JWT_SIGNING_KEY not set; generating a random one for this deploy..."
    JWT_SIGNING_KEY=$(openssl rand -hex 32 2>/dev/null || od -vN 32 -An -tx1 /dev/urandom | tr -d ' \n' | head -c 64)
fi

echo "========================================="
echo " Deploying $SERVICE_NAME to Cloud Run"
echo "  Project: $GCP_PROJECT"
echo "  Region:  $GCP_REGION"
echo "========================================="

gcloud config set project "$GCP_PROJECT" --quiet

# Dedicated minimal service account (security best practice).
SA_NAME="hello-mcp-runner"
SA_EMAIL="$SA_NAME@$GCP_PROJECT.iam.gserviceaccount.com"
if ! gcloud iam service-accounts describe "$SA_EMAIL" &>/dev/null; then
    echo "Creating service account $SA_EMAIL..."
    gcloud iam service-accounts create "$SA_NAME" \
        --display-name="hello-spark-mcp runner" --quiet
fi
# No extra IAM roles needed: this demo stores nothing external.

# CACHE_BUSTER forces a fresh container build even if source is unchanged.
gcloud run deploy "$SERVICE_NAME" \
    --source "$PROJECT_ROOT" \
    --region "$GCP_REGION" \
    --memory "128Mi" \
    --cpu "1" \
    --port "8080" \
    --service-account "$SA_EMAIL" \
    --allow-unauthenticated \
    --session-affinity \
    --set-env-vars "JWT_SIGNING_KEY=$JWT_SIGNING_KEY,CACHE_BUSTER=$(date +%s)"

URL=$(gcloud run services describe "$SERVICE_NAME" --region "$GCP_REGION" --format="value(status.url)")
echo ""
echo "========================================="
echo " Deployed!"
echo "  Service URL:  $URL"
echo "  MCP endpoint: $URL/mcp   (or paste the bare $URL into Spark)"
echo "  PRM:          $URL/.well-known/oauth-protected-resource"
echo "========================================="
echo "Paste the Service URL into Gemini Spark → Connected Apps → custom app."
echo "See docs/connecting-spark.md for the step-by-step."
