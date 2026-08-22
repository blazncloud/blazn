#!/bin/sh

pin_controller_images() {
  agent_manifest=$1
  kueue_manifest=$2
  agent_source=${AGENT_SANDBOX_IMAGE%%@*}
  kueue_source=${KUEUE_IMAGE%%@*}

  agent_count=$(grep -F -c "image: $agent_source" "$agent_manifest" || true)
  kueue_count=$(grep -F -c "image: $kueue_source" "$kueue_manifest" || true)
  [ "$agent_count" -eq 1 ] || { printf 'expected exactly one Agent Sandbox source image, found %s\n' "$agent_count" >&2; return 1; }
  [ "$kueue_count" -eq 1 ] || { printf 'expected exactly one Kueue source image, found %s\n' "$kueue_count" >&2; return 1; }

  sed "s|image: $agent_source|image: $AGENT_SANDBOX_IMAGE|" "$agent_manifest" >"$agent_manifest.pinned"
  sed "s|image: $kueue_source|image: $KUEUE_IMAGE|" "$kueue_manifest" >"$kueue_manifest.pinned"
  mv -- "$agent_manifest.pinned" "$agent_manifest"
  mv -- "$kueue_manifest.pinned" "$kueue_manifest"
  [ "$(grep -F -c "image: $AGENT_SANDBOX_IMAGE" "$agent_manifest")" -eq 1 ]
  [ "$(grep -F -c "image: $KUEUE_IMAGE" "$kueue_manifest")" -eq 1 ]
}

image_id_matches() {
  image_id=$1
  expected_ref=$2
  expected_digest=${expected_ref##*@}
  case "$image_id" in
    *@"$expected_digest") return 0 ;;
    *) printf 'running imageID %s does not match %s\n' "$image_id" "$expected_digest" >&2; return 1 ;;
  esac
}
