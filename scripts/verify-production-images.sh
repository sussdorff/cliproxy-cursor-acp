#!/usr/bin/env bash
set -euo pipefail

mode="${1:-}"
case "${mode}" in
  pre-deploy|post-deploy) shift ;;
  *) echo "usage: $0 <pre-deploy|post-deploy> --compose-file <file> [--compose-file <override>] [--fixture-dir <dir>]" >&2; exit 2 ;;
esac
fixture_dir=""
compose_files=()
while test "$#" -gt 0; do
  case "$1" in
    --fixture-dir) fixture_dir="${2:-}"; shift 2 ;;
    --compose-file) compose_files+=("${2:-}"); shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
test "${#compose_files[@]}" -gt 0 || { echo "at least one --compose-file is required" >&2; exit 2; }
for compose_file in "${compose_files[@]}"; do
  test -f "${compose_file}" || { echo "compose file not found: ${compose_file}" >&2; exit 2; }
done
if test -n "${fixture_dir}"; then test -d "${fixture_dir}" || { echo "fixture directory is missing" >&2; exit 2; }; fi

require_full_sha() {
  printf '%s' "$1" | grep -Eq '^[0-9a-f]{40}$' || { echo "$2 must be a full 40-character commit revision" >&2; return 1; }
}
render_compose_json() {
  if test -n "${fixture_dir}"; then cat "${fixture_dir}/compose.json"; return; fi
  command -v jq >/dev/null 2>&1 || { echo "jq is required to validate rendered Compose service mappings" >&2; return 1; }
  local args=()
  for compose_file in "${compose_files[@]}"; do args+=(--file "${compose_file}"); done
  docker compose "${args[@]}" config --format json
}
validate_compose() {
  local rendered cli_image cpamp_image
  rendered="$(render_compose_json)"
  command -v jq >/dev/null 2>&1 || { echo "jq is required to validate rendered Compose service mappings" >&2; return 1; }
  cli_image="$(printf '%s' "${rendered}" | jq -er '.services["cli-proxy-api"].image')" || { echo "rendered Compose configuration is missing service cli-proxy-api" >&2; return 1; }
  cpamp_image="$(printf '%s' "${rendered}" | jq -er '.services["cpa-manager-plus"].image')" || { echo "rendered Compose configuration is missing service cpa-manager-plus" >&2; return 1; }
  test "${cli_image}" = "${CLI_PROXY_IMAGE}" || { echo "rendered Compose service cli-proxy-api has unapproved image ${cli_image}" >&2; return 1; }
  test "${cpamp_image}" = "${CPAMP_IMAGE}" || { echo "rendered Compose service cpa-manager-plus has unapproved image ${cpamp_image}" >&2; return 1; }
}
inspect_container() {
  local key="$1" container="$2"
  if test -n "${fixture_dir}"; then tr -d '\n' < "${fixture_dir}/${key}.container"; else docker inspect --type container --format '{{.Config.Image}}|{{.Image}}' "${container}"; fi
}
inspect_labels() {
  local key="$1" image_ref="$2"
  if test -n "${fixture_dir}"; then cat "${fixture_dir}/${key}.labels"; else docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.source"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{index .Config.Labels "org.opencontainers.image.version"}}|{{index .Config.Labels "org.opencontainers.image.created"}}' "${image_ref}"; fi
}
inspect_repo_digests() {
  local key="$1" image_ref="$2"
  if test -n "${fixture_dir}"; then cat "${fixture_dir}/${key}.repo-digests"; else docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "${image_ref}"; fi
}
verify_provenance() {
  local key="$1" image_name="$2" source="$3" image_ref="$4" configured_ref="$5" expected_revision="$6"
  local labels actual_source actual_revision version created expected_digest
  labels="$(inspect_labels "${key}" "${image_ref}")"
  IFS='|' read -r actual_source actual_revision version created <<< "${labels}"
  test "${actual_source}" = "${source}" || { echo "${key}: wrong OCI source ${actual_source}" >&2; return 1; }
  require_full_sha "${actual_revision}" "${key} OCI revision"
  test "${actual_revision}" = "${expected_revision}" || { echo "${key}: OCI revision does not match the approved revision" >&2; return 1; }
  test "${version}" = "${expected_revision}" || { echo "${key}: OCI version must identify the full revision" >&2; return 1; }
  test -n "${created}" || { echo "${key}: OCI created label is missing" >&2; return 1; }
  expected_digest="${configured_ref#*@}"
  inspect_repo_digests "${key}" "${image_ref}" | grep -Fxq "ghcr.io/sussdorff/${image_name}@${expected_digest}" || { echo "${key}: image digest does not match the configured digest" >&2; return 1; }
}
verify_image() {
  local key="$1" image_name="$2" source="$3" configured_ref="$4" container="$5" expected_revision="$6"
  local ref_pattern="^ghcr\\.io/sussdorff/${image_name}@sha256:[0-9a-f]{64}$"
  printf '%s' "${configured_ref}" | grep -Eq "${ref_pattern}" || { echo "${key}: configured image must be the approved GHCR image pinned by digest" >&2; return 1; }
  require_full_sha "${expected_revision}" "${key} expected revision"
  if test "${mode}" = "pre-deploy"; then
    if test -z "${fixture_dir}"; then docker pull "${configured_ref}" >/dev/null; fi
    verify_provenance "${key}" "${image_name}" "${source}" "${configured_ref}" "${configured_ref}" "${expected_revision}"
  else
    local state running_ref image_id
    state="$(inspect_container "${key}" "${container}")"
    IFS='|' read -r running_ref image_id <<< "${state}"
    test "${running_ref}" = "${configured_ref}" || { echo "${key}: configured/running image reference mismatch" >&2; return 1; }
    verify_provenance "${key}" "${image_name}" "${source}" "${image_id}" "${configured_ref}" "${expected_revision}"
  fi
  echo "${key}: ${mode} verified ${expected_revision} at ${configured_ref#*@}"
}

validate_requested_image() {
  local key="$1" image_name="$2" configured_ref="$3" expected_revision="$4"
  local ref_pattern="^ghcr\\.io/sussdorff/${image_name}@sha256:[0-9a-f]{64}$"
  printf '%s' "${configured_ref}" | grep -Eq "${ref_pattern}" || { echo "${key}: configured image must be the approved GHCR image pinned by digest" >&2; return 1; }
  require_full_sha "${expected_revision}" "${key} expected revision"
}

: "${CLI_PROXY_IMAGE:?set CLI_PROXY_IMAGE to the digest-pinned production reference}"
: "${CLI_PROXY_REVISION:?set CLI_PROXY_REVISION to the approved full commit revision}"
: "${CPAMP_IMAGE:?set CPAMP_IMAGE to the digest-pinned production reference}"
: "${CPAMP_REVISION:?set CPAMP_REVISION to the approved full commit revision}"
validate_requested_image cli-proxy-api cli-proxy-api "${CLI_PROXY_IMAGE}" "${CLI_PROXY_REVISION}"
validate_requested_image cpa-manager-plus cpa-manager-plus "${CPAMP_IMAGE}" "${CPAMP_REVISION}"
validate_compose
verify_image cli-proxy-api cli-proxy-api https://github.com/sussdorff/CLIProxyAPI "${CLI_PROXY_IMAGE}" "${CLI_PROXY_CONTAINER:-cli-proxy-api}" "${CLI_PROXY_REVISION}"
verify_image cpa-manager-plus cpa-manager-plus https://github.com/sussdorff/CPA-Manager-Plus "${CPAMP_IMAGE}" "${CPAMP_CONTAINER:-cpa-manager-plus}" "${CPAMP_REVISION}"
