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
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

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
  Darwin) test_os=darwin ;;
  Linux) test_os=linux ;;
  *) fail "test host OS is unsupported" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) test_arch=amd64 ;;
  arm64|aarch64) test_arch=arm64 ;;
  *) fail "test host architecture is unsupported" ;;
esac

test_asset="blazn_1.2.3_${test_os}_${test_arch}.tar.gz"
test_version=v1.2.3
test_dist="$test_root/dist"
test_release="$test_dist/download/$test_version"
test_install="$test_root/install"
mkdir -p "$test_release" "$test_install" "$test_root/payload"

cat > "$test_root/payload/blazn" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "version" ]; then
  printf '{"version":"v1.2.3","commit":"test","buildTime":"test","goos":"test","goarch":"test","contractVersion":"v1alpha1"}\n'
  exit 0
fi
printf 'blazn test v1.2.3\n'
EOF
chmod 0755 "$test_root/payload/blazn"
(cd "$test_root/payload" && tar -czf "$test_release/$test_asset" blazn)

ssh-keygen -q -t ed25519 -N '' -f "$test_root/signing_key"
test_fingerprint=$(ssh-keygen -lf "$test_root/signing_key.pub" -E sha256 | awk '{print $2}')
printf 'blazn-release namespaces="blazn-release" %s\n' "$(cat "$test_root/signing_key.pub")" > "$test_root/allowed_signers"

sign_manifest() {
  rm -f "$test_release/SHA256SUMS.sig"
  ssh-keygen -q -Y sign -f "$test_root/signing_key" -n blazn-release "$test_release/SHA256SUMS"
}

write_manifest() {
  printf '%s  %s\n' "$(sha256_file "$test_release/$test_asset")" "$test_asset" > "$test_release/SHA256SUMS"
  sign_manifest
}

run_installer() {
  BLAZN_ALLOW_INSECURE_TEST_ORIGIN=1 \
  BLAZN_DIST_URL="file://$test_dist" \
  BLAZN_VERSION="$test_version" \
  BLAZN_INSTALL_DIR="$test_install" \
  BLAZN_ALLOWED_SIGNERS="$test_root/allowed_signers" \
  BLAZN_SIGNING_FINGERPRINT="$test_fingerprint" \
    sh "$test_repo_root/scripts/install.sh"
}

run_installer_bad_fingerprint() {
  BLAZN_ALLOW_INSECURE_TEST_ORIGIN=1 \
  BLAZN_DIST_URL="file://$test_dist" \
  BLAZN_VERSION="$test_version" \
  BLAZN_INSTALL_DIR="$test_install" \
  BLAZN_ALLOWED_SIGNERS="$test_root/allowed_signers" \
  BLAZN_SIGNING_FINGERPRINT='SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' \
    sh "$test_repo_root/scripts/install.sh"
}

run_installer_fault() {
  BLAZN_ALLOW_INSECURE_TEST_ORIGIN=1 \
  BLAZN_DIST_URL="file://$test_dist" \
  BLAZN_VERSION="$test_version" \
  BLAZN_INSTALL_DIR="$test_install" \
  BLAZN_ALLOWED_SIGNERS="$test_root/allowed_signers" \
  BLAZN_SIGNING_FINGERPRINT="$test_fingerprint" \
  BLAZN_TEST_FAIL_STEP=$1 \
    sh "$test_repo_root/scripts/install.sh"
}

run_installer_missing_command() {
  BLAZN_ALLOW_INSECURE_TEST_ORIGIN=1 \
  BLAZN_DIST_URL="file://$test_dist" \
  BLAZN_VERSION="$test_version" \
  BLAZN_INSTALL_DIR="$test_install" \
  BLAZN_ALLOWED_SIGNERS="$test_root/allowed_signers" \
  BLAZN_SIGNING_FINGERPRINT="$test_fingerprint" \
  BLAZN_TEST_MISSING_COMMAND=$1 \
    sh "$test_repo_root/scripts/install.sh"
}

run_installer_restore_fault() {
  BLAZN_ALLOW_INSECURE_TEST_ORIGIN=1 \
  BLAZN_DIST_URL="file://$test_dist" \
  BLAZN_VERSION="$test_version" \
  BLAZN_INSTALL_DIR="$test_install" \
  BLAZN_ALLOWED_SIGNERS="$test_root/allowed_signers" \
  BLAZN_SIGNING_FINGERPRINT="$test_fingerprint" \
  BLAZN_TEST_FAIL_STEP=after-backup \
  BLAZN_TEST_FAIL_RESTORE=$1 \
    sh "$test_repo_root/scripts/install.sh"
}

inode_of() {
  if [ "$test_os" = "darwin" ]; then
    stat -f '%i' "$1"
  else
    stat -c '%i' "$1"
  fi
}

process_start_of() {
  if [ -r "/proc/$1/stat" ]; then
    sed 's/^.*) //' "/proc/$1/stat" | awk '{print $20}'
  else
    LC_ALL=C TZ=UTC ps -p "$1" -o lstart= | awk '{$1=$1; print}'
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

if run_installer_bad_fingerprint >"$test_root/fingerprint.out" 2>&1; then
  fail "wrong signing fingerprint was accepted"
fi
grep -q 'exactly the expected signing key' "$test_root/fingerprint.out" || fail "fingerprint failure is explicit"
pass "wrong signing fingerprint is rejected"

cp "$test_release/SHA256SUMS" "$test_root/good-checksums"
printf '# tampered\n' >> "$test_release/SHA256SUMS"
if run_installer >"$test_root/tampered-signature.out" 2>&1; then
  fail "tampered signed manifest was accepted"
fi
grep -q 'signature verification failed' "$test_root/tampered-signature.out" || fail "signature failure is explicit"
cp "$test_root/good-checksums" "$test_release/SHA256SUMS"
sign_manifest
pass "tampered manifest is rejected"

cp "$test_release/SHA256SUMS" "$test_root/duplicate-checksums"
cat "$test_root/duplicate-checksums" >> "$test_release/SHA256SUMS"
sign_manifest
if run_installer >"$test_root/duplicate.out" 2>&1; then
  fail "duplicate checksum entry was accepted"
fi
grep -q 'exactly one entry' "$test_root/duplicate.out" || fail "duplicate checksum failure is explicit"
write_manifest
pass "duplicate checksum entry is rejected"

printf '%064d  some-other-asset.tar.gz\n' 0 > "$test_release/SHA256SUMS"
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

printf 'unexpected\n' > "$test_root/payload/unexpected"
(cd "$test_root/payload" && tar -czf "$test_release/$test_asset" blazn unexpected)
write_manifest
if run_installer >"$test_root/unsafe-archive.out" 2>&1; then
  fail "archive with an unexpected path was accepted"
fi
grep -q 'must contain only the blazn binary' "$test_root/unsafe-archive.out" || fail "unsafe archive failure is explicit"
[ "$("$test_install/blazn")" = "blazn test v1.2.3" ] || fail "unsafe archive replaced prior binary"
pass "unexpected archive paths are rejected"

(cd "$test_root/payload" && tar -czf "$test_release/$test_asset" blazn)
write_manifest

cp "$test_install/.blazn-install-receipt" "$test_root/owned-receipt"
rm "$test_install/.blazn-install-receipt"
if run_installer >"$test_root/unowned.out" 2>&1; then
  fail "unreceipted existing binary was replaced"
fi
grep -q 'not owned by a valid direct-install receipt' "$test_root/unowned.out" || fail "unowned binary failure is explicit"
[ "$("$test_install/blazn")" = "blazn test v1.2.3" ] || fail "unowned binary changed"
cp "$test_root/owned-receipt" "$test_install/.blazn-install-receipt"
pass "unreceipted existing binary is refused"

cp "$test_install/blazn" "$test_root/owned-binary"
printf '# modified\n' >> "$test_install/blazn"
if run_installer >"$test_root/modified-existing.out" 2>&1; then
  fail "modified existing binary was replaced"
fi
grep -q 'differs from its receipt' "$test_root/modified-existing.out" || fail "modified binary failure is explicit"
cp "$test_root/owned-binary" "$test_install/blazn"
chmod 0755 "$test_install/blazn"
pass "receipt checksum mismatch is refused"

prepare_upgrade_state() {
  awk -F= '$1 == "version" { print "version=v1.2.2"; next } { print }' \
    "$test_root/owned-receipt" > "$test_install/.blazn-install-receipt"
}

assert_upgrade_rolled_back() {
  [ "$("$test_install/blazn")" = "blazn test v1.2.3" ] || fail "$1 changed the prior binary"
  grep -q '^version=v1.2.2$' "$test_install/.blazn-install-receipt" || fail "$1 did not restore the prior receipt"
}

for fault in after-backup after-binary-install after-receipt-install signal-after-backup signal-after-binary-install; do
  prepare_upgrade_state
  if run_installer_fault "$fault" >"$test_root/fault-$fault.out" 2>&1; then
    fail "fault $fault unexpectedly succeeded"
  fi
  assert_upgrade_rolled_back "$fault"
done
cp "$test_root/owned-receipt" "$test_install/.blazn-install-receipt"
pass "post-backup failures and signals restore the prior installation"

for fault in kill-after-backup kill-after-binary-install; do
  prepare_upgrade_state
  if run_installer_fault "$fault" >"$test_root/fault-$fault.out" 2>&1; then
    fail "kill fault $fault unexpectedly succeeded"
  fi
  run_installer >"$test_root/recover-$fault.out"
  [ "$("$test_install/blazn")" = "blazn test v1.2.3" ] || fail "$fault recovery did not install the candidate"
  grep -q '^version=v1.2.3$' "$test_install/.blazn-install-receipt" || fail "$fault recovery left stale receipt state"
done
pass "SIGKILL residue is reconciled on the next installer run"

prepare_upgrade_state
if run_installer_restore_fault binary >"$test_root/restore-failure.out" 2>&1; then
  fail "injected backup restore failure unexpectedly succeeded"
fi
[ -f "$test_install/.blazn-install.lock" ] || fail "restore failure did not preserve lifecycle lock"
[ -f "$test_install/.blazn-install.journal" ] || fail "restore failure did not preserve recovery journal"
[ -f "$test_install/.blazn.backup" ] || fail "restore failure did not preserve binary backup"
run_installer >"$test_root/recover-restore-failure.out"
[ "$("$test_install/blazn")" = "blazn test v1.2.3" ] || fail "restore-failure recovery did not install candidate"
pass "restore failure preserves and later reconciles recovery metadata"

cp "$test_release/SHA256SUMS.sig" "$test_root/good-signature"
rm "$test_release/SHA256SUMS.sig"
if run_installer >"$test_root/missing-signature.out" 2>&1; then
  fail "missing signature was accepted"
fi
cp "$test_root/good-signature" "$test_release/SHA256SUMS.sig"
pass "missing signature is rejected"

if run_installer_missing_command ssh-keygen >"$test_root/missing-verifier.out" 2>&1; then
  fail "missing signature verifier was accepted"
fi
grep -q 'required command not found: ssh-keygen' "$test_root/missing-verifier.out" || fail "missing verifier failure is explicit"
pass "missing signature verifier is rejected"

stale_owner="$test_install/.blazn-stale-owner"
{
  printf 'pid=99999999\n'
  printf 'start=stale\n'
} > "$stale_owner"
ln "$stale_owner" "$test_install/.blazn-install.lock"
rm "$stale_owner"
{
  printf 'state=uninstall_preparing\n'
  printf 'had_binary=1\n'
  printf 'had_receipt=1\n'
} > "$test_install/.blazn-install.journal"
run_installer >"$test_root/recover-uninstall-prestage.out"
[ -f "$test_install/blazn" ] && [ -f "$test_install/.blazn-install-receipt" ] || fail "pre-stage uninstall recovery damaged owned pair"
[ ! -e "$test_install/.blazn-install.lock" ] && [ ! -e "$test_install/.blazn-install.journal" ] || fail "pre-stage uninstall recovery left lock state"
pass "pre-stage uninstall crash reconciles as a no-op"

active_lock_candidate="$test_install/.blazn-active-lock-owner"
{
  printf 'pid=%s\n' "$$"
  printf 'start=%s\n' "$(process_start_of "$$")"
} > "$active_lock_candidate"
ln "$active_lock_candidate" "$test_install/.blazn-install.lock"
rm "$active_lock_candidate"
if run_installer >"$test_root/lifecycle-lock.out" 2>&1; then
  fail "concurrent lifecycle lock was ignored"
fi
grep -q 'another Blazn install or uninstall' "$test_root/lifecycle-lock.out" || fail "lifecycle lock failure is explicit"
rm "$test_install/.blazn-install.lock"
pass "concurrent lifecycle operation is rejected"

cat > "$test_root/payload/blazn" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "version" ]; then
  printf '{"version":"v9.9.9","commit":"test","buildTime":"test","goos":"test","goarch":"test","contractVersion":"v1alpha1"}\n'
  exit 0
fi
printf 'wrong version\n'
EOF
chmod 0755 "$test_root/payload/blazn"
(cd "$test_root/payload" && tar -czf "$test_release/$test_asset" blazn)
write_manifest
if run_installer >"$test_root/version-mismatch.out" 2>&1; then
  fail "downloaded version mismatch was accepted"
fi
grep -q 'binary version does not match' "$test_root/version-mismatch.out" || fail "version mismatch failure is explicit"
[ "$("$test_install/blazn")" = "blazn test v1.2.3" ] || fail "version mismatch replaced prior binary"
pass "downloaded binary version mismatch is rejected"

printf '1..18\n'
