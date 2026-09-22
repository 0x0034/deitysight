#!/bin/sh
# Disposable local-only S3 test. Requires Docker, OpenSSL and Go 1.25+.
set -eu
project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
run_dir=$(mktemp -d /tmp/deitysight-s3.XXXXXX)
container_id=""
cleanup() {
  if [ -n "$container_id" ]; then
    docker rm -f "$container_id" >/dev/null
  fi
  printf 'Local test certificates remain in %s (valid for two days).\n' "$run_dir"
}
trap cleanup EXIT INT TERM
mkdir -m 0700 "$run_dir/certs"
openssl req -x509 -newkey rsa:2048 -sha256 -days 2 -nodes \
  -keyout "$run_dir/certs/private.key" -out "$run_dir/certs/public.crt" \
  -subj '/CN=localhost' -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
container_id=$(docker run -d --user "$(id -u):$(id -g)" \
  --cap-drop ALL --security-opt no-new-privileges --memory 512m --cpus 1 \
  --tmpfs /data:rw,size=256m,mode=1777 -p 127.0.0.1::9000 \
  -v "$run_dir/certs:/certs:ro" \
  -e MINIO_ROOT_USER=deitysight-integration \
  -e MINIO_ROOT_PASSWORD=fixture-only-minio-password \
  quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z \
  server /data --certs-dir /certs --address :9000)
address=$(docker port "$container_id" 9000/tcp)
attempt=0
until curl --silent --fail --cacert "$run_dir/certs/public.crt" "https://$address/minio/health/ready" >/dev/null; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    docker logs "$container_id"
    exit 1
  fi
  sleep 1
done
cd "$project_dir"
DEITYSIGHT_TEST_S3_ENDPOINT="https://$address" \
DEITYSIGHT_TEST_S3_CA="$run_dir/certs/public.crt" \
DEITYSIGHT_TEST_S3_ACCESS_KEY=deitysight-integration \
DEITYSIGHT_TEST_S3_SECRET_KEY=fixture-only-minio-password \
go test -tags=s3integration ./internal/agent -run '^TestS3LiveHTTPS$' -v -count=1
