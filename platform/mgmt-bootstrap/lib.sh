# lib.sh — shared presentation + helpers for the BOOTSTRAP layer.
#
# Everything that sources this is the thin, imperative launchpad that gets a bare machine (or a fresh
# cluster) to the point where Nephio takes over. It is NOT the product: the 5G lifecycle itself is pure
# KRM (EdgeSite intent -> PackageVariants -> Porch -> Config Sync -> controllers) and touches none of this.
# The helpers exist so that launchpad READS like a product bring-up — clean phases + steps, tool noise
# hidden unless something fails — rather than a wall of apt/helm/kubectl output.

# colors, only on an interactive terminal (honour NO_COLOR)
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  _B=$'\033[1m'; _D=$'\033[2m'; _G=$'\033[32m'; _Y=$'\033[33m'; _R=$'\033[31m'; _C=$'\033[36m'; _0=$'\033[0m'
else _B=''; _D=''; _G=''; _Y=''; _R=''; _C=''; _0=''; fi

# phase <title>  — a named stage of the bootstrap
phase() { printf '\n%s▸ %s%s\n' "${_B}${_C}" "$*" "${_0}"; }
# say <msg>   — dim contextual note        ok/warn <msg> — success / non-fatal note
say()  { printf '   %s%s%s\n' "${_D}" "$*" "${_0}"; }
ok()   { printf '   %s✓%s %s\n' "${_G}" "${_0}" "$*"; }
warn() { printf '   %s!%s %s\n' "${_Y}" "${_0}" "$*"; }
# die <msg>   — fatal: red line + exit 1
die()  { printf '   %s✗ %s%s\n' "${_R}" "$*" "${_0}" >&2; exit 1; }

# run <desc> <cmd...>  — run a side-effect command QUIETLY. Prints "… desc", then rewrites the line to
# "✓ desc" on success (output discarded) or "✗ desc" on failure (captured output shown, indented). Use it
# to wrap the noisy plumbing (apt/helm/docker/kubectl apply); keep output-producing commands direct.
run() {
  local desc="$1"; shift
  local logf; logf="$(mktemp)"
  # in a terminal, show a live "… desc" that gets overwritten by ✓/✗; when piped/captured, just the result.
  [ -t 1 ] && printf '   %s…%s %s' "${_D}" "${_0}" "${desc}"
  if "$@" >"${logf}" 2>&1; then
    [ -t 1 ] && printf '\r'; printf '   %s✓%s %s\n' "${_G}" "${_0}" "${desc}"; rm -f "${logf}"; return 0
  fi
  [ -t 1 ] && printf '\r'; printf '   %s✗%s %s\n' "${_R}" "${_0}" "${desc}"
  sed 's/^/       /' "${logf}" >&2; rm -f "${logf}"; return 1
}
