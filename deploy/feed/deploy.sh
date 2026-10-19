#!/usr/bin/env bash
# Deploys the public video feed (cmd/feed) to Cloud Run with a public GCS bucket.
#
#   deploy/feed/deploy.sh bucket   create/configure the bucket (uniform access, public read, CORS)
#   deploy/feed/deploy.sh deploy   service account, IAM, IP-hash secret, build and deploy
#   deploy/feed/deploy.sh status   show the service, bucket and /healthz
#
# Override with PROJECT, REGION, SERVICE, BUCKET, SA_NAME.
set -euo pipefail

PROJECT="${PROJECT:-moqom-503700}"
REGION="${REGION:-us-central1}"
SERVICE="${SERVICE:-khensu-feed}"
BUCKET="${BUCKET:-khensu-feed}"
SA_NAME="${SA_NAME:-khensu-feed}"
SA="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
SECRET="${SERVICE}-ip-secret"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# GCS CORS matches origins as exact strings ("*" alone is the only wildcard),
# so the *.khensu.nu entry is kept for the record and each product host is
# listed explicitly. Add new hosts here and rerun `bucket`.
ORIGINS=(
  https://khensu.nu
  "https://*.khensu.nu"
  https://avatars.khensu.nu https://aura.khensu.nu https://bi.khensu.nu
  https://create.khensu.nu https://fi.khensu.nu https://indra.og.khensu.nu
  https://media.khensu.nu https://moments.khensu.nu https://network.khensu.nu
  https://memory.khensu.nu https://void.khensu.nu https://waves.khensu.nu
  https://universes.khensu.nu https://shield.khensu.nu https://webe.khensu.nu
  https://tatenan.khensu.nu https://sla.khensu.nu https://kronos.khensu.nu
  http://127.0.0.1:4321 http://localhost:4321
)

g() { gcloud --project="$PROJECT" "$@"; }

cmd_bucket() {
  if ! g storage buckets describe "gs://$BUCKET" >/dev/null 2>&1; then
    g storage buckets create "gs://$BUCKET" --location="$REGION" --uniform-bucket-level-access
  fi
  g storage buckets update "gs://$BUCKET" --uniform-bucket-level-access --no-public-access-prevention
  g storage buckets add-iam-policy-binding "gs://$BUCKET" \
    --member=allUsers --role=roles/storage.objectViewer >/dev/null

  local cors origins=""
  cors="$(mktemp)"
  trap 'rm -f "$cors"' RETURN
  for o in "${ORIGINS[@]}"; do origins+="\"$o\","; done
  cat >"$cors" <<EOF
[{
  "origin": [${origins%,}],
  "method": ["GET", "HEAD", "PUT"],
  "responseHeader": ["Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag"],
  "maxAgeSeconds": 3600
}]
EOF
  g storage buckets update "gs://$BUCKET" --cors-file="$cors"
  echo "bucket gs://$BUCKET ready (public read, CORS for ${#ORIGINS[@]} origins)"
}

cmd_deploy() {
  g services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com \
    iamcredentials.googleapis.com secretmanager.googleapis.com storage.googleapis.com

  if ! g iam service-accounts describe "$SA" >/dev/null 2>&1; then
    g iam service-accounts create "$SA_NAME" --display-name="khensu feed (Cloud Run)"
  fi
  g storage buckets add-iam-policy-binding "gs://$BUCKET" \
    --member="serviceAccount:$SA" --role=roles/storage.objectAdmin >/dev/null
  # Signed upload URLs are signed with signBlob as the service account itself.
  g iam service-accounts add-iam-policy-binding "$SA" \
    --member="serviceAccount:$SA" --role=roles/iam.serviceAccountTokenCreator >/dev/null

  # The daily IP-hash salt derives from this secret. It must be shared by every
  # instance and must not live in the public bucket.
  if ! g secrets describe "$SECRET" >/dev/null 2>&1; then
    head -c 32 /dev/urandom | base64 | tr -d '\n' | g secrets create "$SECRET" --replication-policy=automatic --data-file=-
  fi
  g secrets add-iam-policy-binding "$SECRET" \
    --member="serviceAccount:$SA" --role=roles/secretmanager.secretAccessor >/dev/null

  g run deploy "$SERVICE" \
    --source="$ROOT" \
    --region="$REGION" \
    --service-account="$SA" \
    --allow-unauthenticated \
    --set-env-vars="FEED_STORE=gcs,FEED_BUCKET=$BUCKET,FEED_XFF_HOPS=1" \
    --set-secrets="FEED_IP_SECRET=$SECRET:latest" \
    --cpu=1 --memory=256Mi --concurrency=80 --max-instances=10 --timeout=30s
  cmd_status
}

cmd_status() {
  local url
  url="$(g run services describe "$SERVICE" --region="$REGION" --format='value(status.url)')"
  echo "service $SERVICE: $url"
  g storage buckets describe "gs://$BUCKET" --format='yaml(name,location,uniform_bucket_level_access,public_access_prevention,cors_config)'
  printf 'healthz: '
  curl -fsS "$url/healthz" && echo
}

case "${1:-}" in
  bucket) cmd_bucket ;;
  deploy) cmd_deploy ;;
  status) cmd_status ;;
  *) echo "usage: $0 bucket|deploy|status" >&2; exit 2 ;;
esac
