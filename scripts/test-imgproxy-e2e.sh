#!/usr/bin/env sh
set -eu

compose_file="docker-compose.imgproxy-e2e.yml"
compose_project="bucketmux-imgproxy-e2e"
bucketmux_port="${BUCKETMUX_IMGPROXY_PORT:-18080}"
imgproxy_port="${IMGPROXY_PORT:-18081}"
access_key="imgproxy-access-key"
secret_key="imgproxy-secret-key"
signing_key="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
signing_salt="abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
work_dir="$(mktemp -d)"

cleanup() {
  docker compose --project-name "$compose_project" --file "$compose_file" down --volumes --remove-orphans
  rm -rf "$work_dir"
}
trap cleanup EXIT INT TERM

docker compose --project-name "$compose_project" --file "$compose_file" up --build --detach --wait

python3 - "$work_dir/source.png" <<'PY'
import struct
import sys
import zlib

width, height = 8, 6
rows = []
for y in range(height):
    row = bytearray([0])
    for x in range(width):
        row.extend(((x * 31) % 256, (y * 43) % 256, ((x + y) * 29) % 256))
    rows.append(bytes(row))

def chunk(kind, data):
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xffffffff)

png = b"\x89PNG\r\n\x1a\n"
png += chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
png += chunk(b"IDAT", zlib.compress(b"".join(rows)))
png += chunk(b"IEND", b"")
with open(sys.argv[1], "wb") as output:
    output.write(png)
PY

curl --fail --silent --show-error \
  -X PUT "http://127.0.0.1:${bucketmux_port}/images/imgproxy-source.png" \
  -H "Content-Type: image/png" \
  -H "X-S3LS-Access-Key: ${access_key}" \
  -H "X-S3LS-Secret-Key: ${secret_key}" \
  --data-binary "@${work_dir}/source.png" \
  --output /dev/null

curl --fail --silent --show-error \
  "http://127.0.0.1:${bucketmux_port}/images/imgproxy-source.png" \
  -H "X-S3LS-Access-Key: ${access_key}" \
  -H "X-S3LS-Secret-Key: ${secret_key}" \
  --output "$work_dir/roundtrip.png"
cmp "$work_dir/source.png" "$work_dir/roundtrip.png"

request_path="/rs:fill:4:3/plain/s3://images/imgproxy-source.png@png"
signature="$(python3 - "$signing_key" "$signing_salt" "$request_path" <<'PY'
import base64
import hashlib
import hmac
import sys

key = bytes.fromhex(sys.argv[1])
salt = bytes.fromhex(sys.argv[2])
path = sys.argv[3].encode()
print(base64.urlsafe_b64encode(hmac.new(key, salt + path, hashlib.sha256).digest()).rstrip(b"=").decode())
PY
)"

curl --fail --silent --show-error \
  --dump-header "$work_dir/headers.txt" \
  "http://127.0.0.1:${imgproxy_port}/${signature}${request_path}" \
  --output "$work_dir/transformed.png"

python3 - "$work_dir/headers.txt" "$work_dir/transformed.png" <<'PY'
import struct
import sys

headers = open(sys.argv[1], encoding="utf-8").read().lower()
if "content-type: image/png" not in headers:
    raise SystemExit("imgproxy did not return image/png")

with open(sys.argv[2], "rb") as image:
    data = image.read(24)
if data[:8] != b"\x89PNG\r\n\x1a\n":
    raise SystemExit("imgproxy output is not a PNG")
width, height = struct.unpack(">II", data[16:24])
if (width, height) != (4, 3):
    raise SystemExit(f"unexpected transformed dimensions: {width}x{height}")
with open(sys.argv[2], "rb") as image:
    output_size = len(image.read())
print(f"imgproxy v4.0.14 compatibility certified: S3 SigV4 GET and signed 4x3 PNG transformation ({output_size} bytes)")
PY
