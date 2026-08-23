#!/bin/sh

identity_fail() { printf '%s\n' "$1" >&2; exit "${2:-73}"; }

identity_require_root_file() {
  identity_file=$1
  if [ ! -f "$identity_file" ] || [ -L "$identity_file" ]; then
    identity_fail "required file is not a regular non-symlink: $identity_file"
  fi
  identity_metadata=$(stat -c '%u:%a:%h' -- "$identity_file")
  case "$identity_metadata" in 0:400:1|0:500:1|0:600:1|0:700:1) ;; *) identity_fail "file owner, mode, or link count is unsafe: $identity_file ($identity_metadata)" ;; esac
}

identity_validate_path() {
  identity_path=$1; identity_kind=$2
  case "$identity_path" in /*) ;; *) identity_fail "$identity_kind path must be absolute" 64 ;; esac
  [ "$identity_path" != / ] || identity_fail "$identity_kind path cannot be filesystem root"
  case "$identity_path" in *//*|*/./*|*/../*|*/.|*/..) identity_fail "$identity_kind path is not clean" ;; esac
  identity_clean=$(readlink -m -- "$identity_path")
  [ "$identity_clean" = "$identity_path" ] || identity_fail "$identity_kind path is not canonical"
  identity_component=/
  identity_remaining=${identity_path#/}
  identity_old_ifs=$IFS; IFS=/
  for identity_part in $identity_remaining; do
    [ -n "$identity_part" ] || continue
    identity_component=${identity_component%/}/$identity_part
    [ ! -L "$identity_component" ] || { IFS=$identity_old_ifs; identity_fail "$identity_kind path has a symlink component: $identity_component"; }
    if [ -e "$identity_component" ]; then
	  if { [ "$identity_kind" = driver ] || [ "$identity_kind" = patarchive ]; } && [ "$identity_component" = "$identity_path" ]; then
		identity_require_root_file "$identity_component"
		continue
	  fi
      [ -d "$identity_component" ] || { IFS=$identity_old_ifs; identity_fail "$identity_kind path has a non-directory component: $identity_component"; }
      if find "$identity_component" -maxdepth 0 ! -user root -print -quit | grep -q .; then IFS=$identity_old_ifs; identity_fail "$identity_kind path component is not root-owned: $identity_component"; fi
      if find "$identity_component" -maxdepth 0 -perm /022 ! -perm -1000 -print -quit | grep -q .; then IFS=$identity_old_ifs; identity_fail "$identity_kind path component is writable without root-owned sticky protection: $identity_component"; fi
    fi
  done
  IFS=$identity_old_ifs
  case "$identity_kind:$identity_path" in
	data:/var/lib/blazn/identity|secrets:/etc/blazn/identity/secrets|backup:/srv/backups/blazn/identity/*|receipt:/var/lib/blazn/identity-qualification/*|driver:/usr/libexec/blazn/identity-qualification-driver|recovery:/var/lib/blazn/identity.pre-restore.*) ;;
	patarchive:/srv/backups/blazn/identity/*/zitadel-bootstrap.tar|patarchive:/var/lib/blazn/identity.pre-restore.*/pre-restore-pat.tar) ;;
	data:/tmp/blazn-identity-disposable.*/data|secrets:/tmp/blazn-identity-disposable.*/secrets|backup:/tmp/blazn-identity-disposable.*/backup|receipt:/tmp/blazn-identity-qualification.*.json|driver:/tmp/blazn-identity-qualification-driver.*|recovery:/tmp/blazn-identity-disposable.*/recovery|patarchive:/tmp/blazn-identity-disposable.*/*/*.tar) ;;
    *) identity_fail "$identity_kind path is outside its fixed approved prefix: $identity_path" ;;
  esac
}

identity_reject_overlap() {
  identity_left=$1; identity_right=$2
  [ "$identity_left" != "$identity_right" ] || identity_fail "identity roots must be distinct and non-overlapping"
  case "$identity_right" in "$identity_left"/*) identity_fail "identity roots must be distinct and non-overlapping" ;; esac
  case "$identity_left" in "$identity_right"/*) identity_fail "identity roots must be distinct and non-overlapping" ;; esac
}
