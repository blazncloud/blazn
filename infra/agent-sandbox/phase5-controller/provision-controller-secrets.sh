#!/bin/sh
set -eu

# Provisions the two Secrets the Phase 5 sandbox controller consumes, into
# blazn-poc-system: the controller database URL and the object-store
# credentials. Reads the sensitive values from root-only files, rewrites the
# database URL host/port to the reviewed reachable endpoint, and applies the
# Secrets server-side. Never prints any secret value.
[ "$#" -eq 0 ] || { printf 'usage: %s\n' "$0" >&2; exit 64; }
: "${BLAZN_PHASE5_TRANSACTION_ID:?set the boundary transaction UUID for ownership annotation}"
: "${BLAZN_CONTROLLER_DATABASE_URL_FILE:?set a root-only file holding the controller database URL}"
: "${BLAZN_OBJECT_ACCESS_KEY_FILE:?set a root-only file holding the object access key}"
: "${BLAZN_OBJECT_SECRET_KEY_FILE:?set a root-only file holding the object secret key}"
: "${BLAZN_DATABASE_URL_SECRET_NAME:?set the database URL Secret name}"
: "${BLAZN_DATABASE_URL_SECRET_KEY:?set the database URL Secret key}"
: "${BLAZN_OBJECT_SECRET_NAME:?set the object credential Secret name}"
: "${BLAZN_OBJECT_ACCESS_KEY:?set the object access Secret key}"
: "${BLAZN_OBJECT_SECRET_KEY:?set the object secret Secret key}"
: "${BLAZN_OBJECT_CA_CERT_FILE:?set a root-only file holding the object endpoint CA certificate}"
: "${BLAZN_OBJECT_CA_KEY:?set the object CA Secret key}"
: "${BLAZN_BEN1_POSTGRES_HOST:?set the reachable controller database host}"
: "${BLAZN_BEN1_POSTGRES_PORT:?set the reachable controller database port}"
[ "$BLAZN_OBJECT_ACCESS_KEY" != "$BLAZN_OBJECT_SECRET_KEY" ] || { printf 'object credential keys must be distinct\n' >&2; exit 1; }
if [ "$BLAZN_OBJECT_CA_KEY" = "$BLAZN_OBJECT_ACCESS_KEY" ] || [ "$BLAZN_OBJECT_CA_KEY" = "$BLAZN_OBJECT_SECRET_KEY" ]; then
  printf 'object credential keys must be distinct\n' >&2
  exit 1
fi
for required in kubectl python3 openssl; do command -v "$required" >/dev/null 2>&1 || { printf '%s is required\n' "$required" >&2; exit 1; }; done
for sensitive in "$BLAZN_CONTROLLER_DATABASE_URL_FILE" "$BLAZN_OBJECT_ACCESS_KEY_FILE" "$BLAZN_OBJECT_SECRET_KEY_FILE" "$BLAZN_OBJECT_CA_CERT_FILE"; do
  if ! { [ -f "$sensitive" ] && [ ! -L "$sensitive" ]; }; then printf 'sensitive input is unsafe: %s\n' "$sensitive" >&2; exit 1; fi
done

kubectl get namespace blazn-poc-system >/dev/null

umask 077
work=$(mktemp -d "${TMPDIR:-/tmp}/blazn-controller-secrets.XXXXXX")
cleanup() { find "$work" -xdev -type f -delete; find "$work" -xdev -depth -type d -empty -delete; }
trap cleanup EXIT HUP INT TERM

# Rewrite the database URL authority to the reachable endpoint without ever
# emitting the URL (which carries the password) to stdout/stderr or argv.
BLAZN_CONTROLLER_DATABASE_URL_FILE="$BLAZN_CONTROLLER_DATABASE_URL_FILE" \
BLAZN_BEN1_POSTGRES_HOST="$BLAZN_BEN1_POSTGRES_HOST" \
BLAZN_BEN1_POSTGRES_PORT="$BLAZN_BEN1_POSTGRES_PORT" \
python3 - "$work/database-url" <<'PY'
import os, re, sys
raw = open(os.environ["BLAZN_CONTROLLER_DATABASE_URL_FILE"]).read().strip()
host = os.environ["BLAZN_BEN1_POSTGRES_HOST"]
port = os.environ["BLAZN_BEN1_POSTGRES_PORT"]
match = re.match(r"^(?P<scheme>postgres(?:ql)?)://(?P<userinfo>[^@/]+)@(?P<authority>[^/?#]+)(?P<rest>[/?#].*)?$", raw, re.DOTALL)
if not match:
    sys.stderr.write("controller database URL is not a parseable postgres URL\n")
    sys.exit(1)
username = match.group("userinfo").split(":", 1)[0]
if username != "blazn_sandbox_controller":
    sys.stderr.write("controller database URL must authenticate as blazn_sandbox_controller\n")
    sys.exit(1)
rewritten = f'{match.group("scheme")}://{match.group("userinfo")}@{host}:{port}{match.group("rest") or "/"}'
out = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
os.write(out, rewritten.encode())
os.close(out)
PY

install -m 0600 "$BLAZN_OBJECT_ACCESS_KEY_FILE" "$work/object-access"
install -m 0600 "$BLAZN_OBJECT_SECRET_KEY_FILE" "$work/object-secret"
install -m 0600 "$BLAZN_OBJECT_CA_CERT_FILE" "$work/object-ca"

# The controller's hardened CA reader accepts only bare, headerless
# CERTIFICATE PEM blocks; a decorated file (openssl -text preamble,
# subject=/issuer= lines, bundle comments) or a charset-clean but
# unparseable block would pass every provisioning step and then
# crash-loop the controller at startup. Reject both here: shape-check
# every block, then prove each one parses as an X.509 certificate.
BLAZN_OBJECT_CA_WORK_FILE="$work/object-ca" python3 - <<'PY'
import os, re, subprocess, sys
contents = open(os.environ["BLAZN_OBJECT_CA_WORK_FILE"], "rb").read().decode("ascii", "strict").strip()
block = re.compile(
    r"-----BEGIN CERTIFICATE-----\r?\n[A-Za-z0-9+/=\r\n]+-----END CERTIFICATE-----")
remaining, blocks = contents, []
while remaining:
    match = block.match(remaining)
    if not match:
        sys.stderr.write("object CA file must contain only bare CERTIFICATE PEM blocks\n")
        sys.exit(1)
    blocks.append(match.group(0))
    remaining = remaining[match.end():].strip()
if not blocks:
    sys.stderr.write("object CA file holds no certificate\n")
    sys.exit(1)
for pem in blocks:
    parsed = subprocess.run(["openssl", "x509", "-noout"], input=pem.encode(),
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if parsed.returncode != 0:
        sys.stderr.write("object CA file holds an unparseable certificate block\n")
        sys.exit(1)
PY

cat >"$work/annotate.py" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
doc.setdefault("metadata", {}).setdefault("labels", {})["app.kubernetes.io/part-of"] = "blazn-phase5"
doc["metadata"].setdefault("annotations", {})["blazn.dev/phase5-transaction"] = sys.argv[2]
json.dump(doc, open(sys.argv[1], "w"))
PY

apply_secret() {
  apply_manifest=$1
  kubectl apply --server-side --field-manager blazn-phase5-controller -f "$apply_manifest" >/dev/null
}

kubectl create secret generic "$BLAZN_DATABASE_URL_SECRET_NAME" -n blazn-poc-system \
  --from-file="$BLAZN_DATABASE_URL_SECRET_KEY=$work/database-url" --dry-run=client -o json >"$work/db-secret.json"
python3 "$work/annotate.py" "$work/db-secret.json" "$BLAZN_PHASE5_TRANSACTION_ID"
apply_secret "$work/db-secret.json"

kubectl create secret generic "$BLAZN_OBJECT_SECRET_NAME" -n blazn-poc-system \
  --from-file="$BLAZN_OBJECT_ACCESS_KEY=$work/object-access" \
  --from-file="$BLAZN_OBJECT_SECRET_KEY=$work/object-secret" \
  --from-file="$BLAZN_OBJECT_CA_KEY=$work/object-ca" --dry-run=client -o json >"$work/object-secret.json"
python3 "$work/annotate.py" "$work/object-secret.json" "$BLAZN_PHASE5_TRANSACTION_ID"
apply_secret "$work/object-secret.json"

printf 'controller Secrets provisioned in blazn-poc-system\n'
