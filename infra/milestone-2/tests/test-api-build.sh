#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "$TEST_DIR/../../.." && pwd)
command -v sudo >/dev/null 2>&1 || { printf 'API build fault tests skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'API build fault tests skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-api-build-test-$$
mkdir "$top"
cleanup() {
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

cat >"$top/docker" <<'EOF'
#!/bin/sh
set -eu
key() { printf '%s' "$1" | tr '/:' '__'; }
case "$1:$2" in
  compose:-f)
    case "$*" in
      *" build api")
        printf '%064d\n' 1 >"$FAKE_IMAGES/$(key "$CONTROL_API_IMAGE")"
        if [ "${FAKE_MUTATE_SOURCE:-0}" = 1 ]; then printf '\n// concurrent mutation\n' >>"$FAKE_REPO/services/control-api/src/config.ts"; fi
        ;;
      *" ps -a -q "*)
        service=${*##* ps -a -q }
        printf 'container-%s\n' "$service"
        ;;
      *) exit 97 ;;
    esac
    ;;
  image:inspect)
    file=$FAKE_IMAGES/$(key "$3")
    [ -f "$file" ] || exit 1
    printf 'sha256:%s\n' "$(sed -n '1p' "$file")"
    ;;
  image:tag)
    cp "$FAKE_IMAGES/$(key "$3")" "$FAKE_IMAGES/$(key "$4")"
    ;;
  image:rm)
    rm -f "$FAKE_IMAGES/$(key "$3")"
    ;;
  inspect:--format)
    container=${*##* }
    service=${container#container-}
    case "$*" in
      *".State.Status"*)
        case "$service" in api) printf 'running/0\n' ;; *) printf 'exited/0\n' ;; esac
        ;;
      *)
        id=$(sed -n '1p' "$FAKE_IMAGES/$(key "$CONTROL_API_IMAGE")")
        if [ "${FAKE_CONTAINER_MISMATCH:-0}" = 1 ] && [ "$service" = api ]; then id=$(printf %064d 8); fi
        printf 'blazn-m2/%s/sha256:%s\n' "$service" "$id"
        ;;
    esac
    ;;
  *) exit 98 ;;
esac
EOF
chmod 0755 "$top/docker"

fixture() {
  name=$1
  root=$top/$name
  mkdir -p "$root/repo" "$root/images" "$root/ownership"
  cp -R "$REPO_ROOT/infra" "$REPO_ROOT/services" "$REPO_ROOT/packages" "$root/repo/"
  printf 'synthetic env\n' >"$root/control-plane.env"
  chmod 0600 "$root/control-plane.env"
  sudo chown -R 0:0 "$root/ownership" "$root/control-plane.env"
  sudo chmod 0700 "$root/ownership"
  printf '%s\n' "$root"
}

run_build() {
  root=$1
  shift
  sudo env PATH="$top:$PATH" FAKE_IMAGES="$root/images" FAKE_REPO="$root/repo" BLAZN_FENCING_TOKEN=9 \
    BLAZN_CONTROL_PLANE_ENV_FILE="$root/control-plane.env" BLAZN_CONTROL_API_BUILD_RECEIPT="$root/ownership/build.json" \
    "$@" "$root/repo/infra/milestone-2/scripts/build-control-api.sh"
}

clean=$(fixture clean)
run_build "$clean" env >/dev/null
sudo jq -e '.image|test("^blazn-control-api:source-[a-f0-9]{64}$")' "$clean/ownership/build.json" >/dev/null
CONTROL_API_IMAGE=$(sudo jq -r .image "$clean/ownership/build.json")
export CONTROL_API_IMAGE FAKE_IMAGES="$clean/images"
PATH="$top:$PATH"
export PATH
# shellcheck disable=SC1091
. "$clean/repo/infra/milestone-2/scripts/common.sh"
verify_control_api_containers "$clean/repo/infra/milestone-2" "$clean/control-plane.env"
if (FAKE_CONTAINER_MISMATCH=1; export FAKE_CONTAINER_MISMATCH; verify_control_api_containers "$clean/repo/infra/milestone-2" "$clean/control-plane.env") >"$clean/mismatch.out" 2>"$clean/mismatch.err"; then exit 43; fi
grep -F 'does not match its receipt' "$clean/mismatch.err" >/dev/null

race=$(fixture race)
printf sentinel | sudo tee "$race/ownership/build.json" >/dev/null
sudo chown 0:0 "$race/ownership/build.json"
sudo chmod 0600 "$race/ownership/build.json"
if run_build "$race" env FAKE_MUTATE_SOURCE=1 >"$race/out" 2>"$race/err"; then exit 41; fi
grep -F 'source changed during build' "$race/err" >/dev/null
[ "$(sudo cat "$race/ownership/build.json")" = sentinel ]

swap=$(fixture swap)
# shellcheck disable=SC1091
. "$swap/repo/infra/milestone-2/scripts/common.sh"
digest=$(control_api_source_digest "$swap/repo/infra/milestone-2")
final=blazn-control-api_source-$digest
printf '%064d\n' 2 >"$swap/images/$final"
printf sentinel | sudo tee "$swap/ownership/build.json" >/dev/null
sudo chown 0:0 "$swap/ownership/build.json"
sudo chmod 0600 "$swap/ownership/build.json"
if run_build "$swap" env >"$swap/out" 2>"$swap/err"; then exit 42; fi
grep -F 'already resolves to different content' "$swap/err" >/dev/null
[ "$(sudo cat "$swap/ownership/build.json")" = sentinel ]
[ "$(cat "$swap/images/$final")" = "$(printf %064d 2)" ]

trap - EXIT HUP INT TERM
cleanup
printf 'API build stable, TOCTOU, and immutable-tag swap tests passed\n'
