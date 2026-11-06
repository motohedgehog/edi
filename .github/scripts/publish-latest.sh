#!/usr/bin/env bash
set -euo pipefail

: "${IMAGE:?}" "${SOURCE:?}" "${DIGEST:?}" "${IS_RELEASE:?}" "${RUNNER_TEMP:?}" "${GITHUB_STEP_SUMMARY:?}"
[[ "$SOURCE" =~ ^[0-9a-f]{40}$ && "$DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || exit 1
[[ "$IS_RELEASE" == true || "$IS_RELEASE" == false ]] || exit 1

if [[ "$IS_RELEASE" == false ]]; then
  if skopeo --override-os linux --override-arch amd64 inspect --no-tags "docker://$IMAGE:latest" \
    > "$RUNNER_TEMP/latest.json" 2> "$RUNNER_TEMP/latest-error"; then
    if ! jq -se '
      def release: test("\\Av(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\z");
      length == 1 and (.[0] |
        (.Digest | test("\\Asha256:[0-9a-f]{64}\\z"))
        and (.Labels["org.opencontainers.image.revision"] | test("\\A[0-9a-f]{40}\\z"))
        and (.Labels["org.opencontainers.image.version"] as $version |
          ($version | release) or $version == "sha-" + .Labels["org.opencontainers.image.revision"]))
    ' "$RUNNER_TEMP/latest.json" > /dev/null; then
      echo 'Latest image metadata is invalid.' >&2
      exit 1
    fi
    read -r revision version <<< "$(jq -r '[.Labels["org.opencontainers.image.revision"], .Labels["org.opencontainers.image.version"]] | @tsv' "$RUNNER_TEMP/latest.json")"
    if [[ "$revision" == "$SOURCE" && "$version" == v* ]]; then
      printf 'Latest preserved: release %s already represents this source.\n' "$version" >> "$GITHUB_STEP_SUMMARY"
      exit 0
    fi
  else
    absent='manifest unknown|MANIFEST_UNKNOWN|name unknown|NAME_UNKNOWN'
    if ! grep -Eiq "$absent" "$RUNNER_TEMP/latest-error"; then
      cat "$RUNNER_TEMP/latest-error" >&2
      exit 1
    fi
    # Missing child manifests also fail inspection; confirm the top-level tag is absent.
    if skopeo inspect --raw "docker://$IMAGE:latest" > /dev/null 2> "$RUNNER_TEMP/latest-raw-error"; then
      cat "$RUNNER_TEMP/latest-error" >&2
      exit 1
    fi
    if ! grep -Eiq "$absent" "$RUNNER_TEMP/latest-raw-error"; then
      cat "$RUNNER_TEMP/latest-raw-error" >&2
      exit 1
    fi
  fi
fi

main="$(git ls-remote --exit-code origin refs/heads/main | cut -f1)"
[[ "$main" =~ ^[0-9a-f]{40}$ ]] || exit 1
if [[ "$main" != "$SOURCE" ]]; then
  echo 'Latest skipped: source is no longer current main.' >> "$GITHUB_STEP_SUMMARY"
  exit 0
fi
skopeo copy --all --preserve-digests --digestfile "$RUNNER_TEMP/latest.digest" \
  "docker://$IMAGE@$DIGEST" "docker://$IMAGE:latest"
test "$(cat "$RUNNER_TEMP/latest.digest")" = "$DIGEST"
printf 'Latest changed to %s@%s.\n' "$IMAGE" "$DIGEST" >> "$GITHUB_STEP_SUMMARY"
