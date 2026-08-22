#!/bin/sh

set -eu

test_root=$(mktemp -d "${TMPDIR:-/tmp}/blazn-installer-test.XXXXXX")
test_repo_root=$(CDPATH='' cd "$(dirname "$0")/.." && pwd)

cleanup() {
  case "$test_root" in
    "${TMPDIR:-/tmp}"/blazn-installer-test.*) rm -rf "$test_root" ;;
    *) printf 'refusing to remove unexpected test path: %s\n' "$test_root" >&2 ;;
  esac
}
trap cleanup EXIT HUP INT TERM

fail() {
  printf 'not ok - %s\n' "$1" >&2
  exit 1
}

pass() {
  printf 'ok - %s\n' "$1"
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

case "$(uname -s)" in
  Darwin) test_os=Darwin ;;
  Linux) test_os=Linux ;;
  *) fail "test host OS is unsupported" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) test_arch=amd64 ;;
  arm64|aarch64) test_arch=arm64 ;;
  *) fail "test host architecture is unsupported" ;;
esac

test_asset="blazn_${test_os}_${test_arch}.tar.gz"
test_version=v1.2.3
test_dist="$test_root/dist"
test_release="$test_dist/download/$test_version"
test_install="$test_root/install"
mkdir -p "$test_release" "$test_dist/latest/download" "$test_install" "$test_root/payload"
printf '%s\n' "$test_version" > "$test_dist/latest/download/version.txt"

cat > "$test_root/payload/blazn" <<'EOF'
#!/bin/sh
printf 'blazn test v1.2.3\n'
EOF
chmod 0755 "$test_root/payload/blazn"
(cd "$test_root/payload" && tar -czf "$test_release/$test_asset" blazn)

ssh-keygen -q -t ed25519 -N '' -f "$test_root/signing_key"
test_fingerprint=$(ssh-keygen -lf "$test_root/signing_key.pub" -E sha256 | awk '{print $2}')
printf 'release@blazn.dev namespaces="file" %s\n' "$(cat "$test_root/signing_key.pub")" > "$test_root/allowed_signers"

sign_manifest() {
  rm -f "$test_release/checksums.txt.sig"
  ssh-keygen -q -Y sign -f "$test_root/signing_key" -n file "$test_release/checksums.txt"
}

write_manifest() {
  printf '%s  %s\n' "$(sha256_file "$test_release/$test_asset")" "$test_asset" > "$test_release/checksums.txt"
  sign_manifest
}

run_installer() {
  BLAZN_ALLOW_INSECURE_TEST_ORIGIN=1 \
  BLAZN_DIST_URL="file://$test_dist" \
  BLAZN_INSTALL_DIR="$test_install" \
  BLAZN_ALLOWED_SIGNERS="$test_root/allowed_signers" \
  BLAZN_SIGNING_FINGERPRINT="$test_fingerprint" \
    sh "$test_repo_root/scripts/install.sh"
}

inode_of() {
  if [ "$test_os" = "Darwin" ]; then
    stat -f '%i' "$1"
  else
    stat -c '%i' "$1"
  fi
}

write_manifest
run_installer >/dev/null
[ -x "$test_install/blazn" ] || fail "signed archive installs executable"
[ "$("$test_install/blazn")" = "blazn test v1.2.3" ] || fail "installed binary runs"
grep -q '^version=v1.2.3$' "$test_install/.blazn-install-receipt" || fail "receipt records version"
pass "signed archive installs with receipt"

first_inode=$(inode_of "$test_install/blazn")
run_installer > "$test_root/idempotent.out"
second_inode=$(inode_of "$test_install/blazn")
[ "$first_inode" = "$second_inode" ] || fail "same version is idempotent"
grep -q 'already installed' "$test_root/idempotent.out" || fail "idempotent result is reported"
pass "same version is idempotent"

cp "$test_release/checksums.txt" "$test_root/good-checksums"
printf '# tampered\n' >> "$test_release/checksums.txt"
if run_installer >"$test_root/tampered-signature.out" 2>&1; then
  fail "tampered signed manifest was accepted"
fi
grep -q 'signature verification failed' "$test_root/tampered-signature.out" || fail "signature failure is explicit"
cp "$test_root/good-checksums" "$test_release/checksums.txt"
sign_manifest
pass "tampered manifest is rejected"

cp "$test_release/checksums.txt" "$test_root/duplicate-checksums"
cat "$test_root/duplicate-checksums" >> "$test_release/checksums.txt"
sign_manifest
if run_installer >"$test_root/duplicate.out" 2>&1; then
  fail "duplicate checksum entry was accepted"
fi
grep -q 'exactly one entry' "$test_root/duplicate.out" || fail "duplicate checksum failure is explicit"
write_manifest
pass "duplicate checksum entry is rejected"

printf '%064d  some-other-asset.tar.gz\n' 0 > "$test_release/checksums.txt"
sign_manifest
if run_installer >"$test_root/missing.out" 2>&1; then
  fail "missing checksum entry was accepted"
fi
grep -q 'exactly one entry' "$test_root/missing.out" || fail "missing checksum failure is explicit"
write_manifest
pass "missing checksum entry is rejected"

printf 'corrupt archive\n' >> "$test_release/$test_asset"
if run_installer >"$test_root/tampered-archive.out" 2>&1; then
  fail "tampered archive was accepted"
fi
grep -q 'checksum mismatch' "$test_root/tampered-archive.out" || fail "archive checksum failure is explicit"
[ "$("$test_install/blazn")" = "blazn test v1.2.3" ] || fail "failed install replaced prior binary"
pass "tampered archive is rejected and prior install survives"

printf '1..6\n'
