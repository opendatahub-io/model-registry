#!/usr/bin/env bash
set -euo pipefail

# Konflux PR build simulator for opendatahub-io/model-registry.
#
# The simulator builds the two images that Konflux builds from this repository
# (.tekton/*pull-request.yaml), checks their runtime behaviour, validates the
# Go/Python dependency inputs that a hermetic build would prefetch, and — in the
# operator phase — deploys the model-registry-operator against the PR image.
#
# Usage:
#   scripts/validate-build.sh <static|go-modules|hermetic|manifests|server|async-upload|operator|all>
#
# Environment:
#   SERVER_IMAGE      Local tag for the model-registry server image
#   ASYNC_IMAGE       Local tag for the async-upload job image
#   PLATFORM          Docker platform to build (default linux/amd64)
#   POSTGRES_IMAGE    Postgres image used by the server runtime test
#   SERVER_PORT       Host port for the server runtime test (default 18080)
#   KUBECONFORM_BIN   kubeconform binary (default kubeconform)
#   KUSTOMIZE_BIN     kustomize binary (default kustomize)
#   KUBERNETES_VERSION  Kubernetes schema version for kubeconform
#   KIND_CLUSTER_NAME   Kind cluster name for the operator phase
#   KIND_NODE_IMAGE     Kind node image (default kindest/node:v1.33.7)
#   OPERATOR_REPO       model-registry-operator git URL
#   OPERATOR_REF        model-registry-operator ref (defaults to a pinned commit)
#   REGISTRY_NAMESPACE  Namespace for the sample ModelRegistry (default default)
#   KEEP_KIND=1         Keep the Kind cluster alive for debugging
#   OPERATOR_SETTLE_SECONDS  Stability window after Available (default 30)

MODE="${1:-all}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"

SERVER_IMAGE="${SERVER_IMAGE:-quay.io/opendatahub/model-registry:pr-validation}"
ASYNC_IMAGE="${ASYNC_IMAGE:-quay.io/opendatahub/model-registry-job-async-upload:pr-validation}"
PLATFORM="${PLATFORM:-linux/amd64}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-public.ecr.aws/docker/library/postgres:16-alpine}"
SERVER_PORT="${SERVER_PORT:-18080}"
KUSTOMIZE_BIN="${KUSTOMIZE_BIN:-kustomize}"
KUBECONFORM_BIN="${KUBECONFORM_BIN:-kubeconform}"
KUBERNETES_VERSION="${KUBERNETES_VERSION:-1.33.0}"

info() { printf 'ℹ️  %s\n' "$*"; }
pass() { printf '✅ %s\n' "$*"; }
warn() { printf '⚠️  %s\n' "$*"; }
err() { printf '❌ %s\n' "$*" >&2; }

STATIC_ISSUES=0

static_error() {
  err "$*"
  STATIC_ISSUES=$((STATIC_ISSUES + 1))
}

require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    err "Required command not found: $cmd"
    return 1
  fi
}

check_konflux_targets() {
  info "Checking Konflux PipelineRun build targets..."
  local found=0
  local pr context dockerfile path

  while IFS= read -r pr; do
    found=$((found + 1))
    context=$(awk '/- name: path-context/{getline; sub(/^[[:space:]]*value:[[:space:]]*/, ""); print; exit}' "$pr")
    dockerfile=$(awk '/- name: dockerfile/{getline; sub(/^[[:space:]]*value:[[:space:]]*/, ""); print; exit}' "$pr")

    if [ -z "$context" ] || [ -z "$dockerfile" ]; then
      static_error "$pr: could not parse path-context/dockerfile parameters"
      continue
    fi

    if [ "$context" = "." ]; then
      path="$dockerfile"
    else
      path="$context/$dockerfile"
    fi

    if [ ! -f "$path" ]; then
      static_error "$pr: Dockerfile not found at $path"
    else
      pass "$pr -> $path"
    fi
  done < <(find .tekton -maxdepth 1 \( -name '*pull-request*.yaml' -o -name '*pull-request*.yml' \) | sort)

  if [ "$found" -eq 0 ]; then
    static_error "No Konflux pull-request PipelineRuns found under .tekton/"
  fi
}

check_go_manifests() {
  info "Checking Go module manifests for hermetic build compatibility..."

  if ! command -v python3 >/dev/null 2>&1; then
    warn "python3 not available; falling back to basic go.sum presence checks"
    while IFS= read -r gomod; do
      if [ ! -f "${gomod%.mod}.sum" ]; then
        static_error "$gomod exists but ${gomod%.mod}.sum is missing"
      fi
    done < <(find . -name go.mod -not -path './.git/*' | sort)
    return 0
  fi

  local output status
  set +e
  output=$(python3 - <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(".").resolve()
errors = 0
warnings = 0
checked = 0


def parse_requires(text):
    requires = []
    in_block = False
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("require ("):
            in_block = True
            continue
        if in_block and stripped == ")":
            in_block = False
            continue
        if in_block:
            match = re.match(r"^(\S+)\s+(v\S+)", stripped)
            if match:
                requires.append((match.group(1), match.group(2)))
        elif stripped.startswith("require "):
            match = re.match(r"^require\s+(\S+)\s+(v\S+)", stripped)
            if match:
                requires.append((match.group(1), match.group(2)))
    return requires


def parse_replaces(text):
    # Go applies `old v1.0.0 => new` only to that exact version, while
    # `old => new` applies to every version. Keep the optional left version so
    # the two cases are not conflated.
    replaces = []
    in_block = False
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("replace ("):
            in_block = True
            continue
        if in_block and stripped == ")":
            in_block = False
            continue
        if in_block:
            parts = stripped.split("=>")
            if len(parts) == 2:
                left = parts[0].strip().split()
                if left:
                    replaces.append(
                        (left[0], left[1] if len(left) > 1 else None, parts[1].strip())
                    )
        elif stripped.startswith("replace "):
            parts = stripped.partition("=>")
            if parts[1]:
                left = parts[0].replace("replace", "", 1).strip().split()
                if left:
                    replaces.append(
                        (left[0], left[1] if len(left) > 1 else None, parts[2].strip())
                    )
    return replaces


def replacement_for(replaces, module, version):
    """Return the replace directive Go applies to module at version, if any."""
    candidates = [
        replace
        for replace in replaces
        if replace[0] == module and replace[1] in (None, version)
    ]
    if not candidates:
        return None
    qualified = [replace for replace in candidates if replace[1] is not None]
    return qualified[-1] if qualified else candidates[-1]


for gomod in sorted(root.rglob("go.mod")):
    if ".git" in gomod.parts:
        continue
    checked += 1
    text = gomod.read_text()
    module_match = re.search(r"^module\s+(\S+)", text, re.M)
    if not module_match:
        print(f"ERROR: {gomod.relative_to(root)}: missing module directive")
        errors += 1
        continue

    requires = parse_requires(text)
    replaces = parse_replaces(text)
    local_replaces = sorted(
        {
            (old, new)
            for old, _version, new in replaces
            if new.startswith("./") or new.startswith("../")
        }
    )

    external_requires = []
    for module, version in requires:
        replaced = replacement_for(replaces, module, version)
        if replaced is not None:
            _old, _old_version, new = replaced
            if new.startswith("./") or new.startswith("../"):
                continue
            fields = new.split()
            if len(fields) == 2:
                # go.sum records the replacement module's checksum, not the original.
                module, version = fields
        external_requires.append((module, version))
    external_requires = sorted(set(external_requires))

    gosum = gomod.with_name("go.sum")
    if external_requires:
        if not gosum.exists():
            print(
                f"ERROR: {gomod.relative_to(root)}: external requirements but go.sum is missing"
            )
            errors += 1
        else:
            sums = gosum.read_text()
            for module, version in external_requires:
                pattern = re.compile(
                    rf"^{re.escape(module)}\s+{re.escape(version)}\s+h1:", re.M
                )
                if not pattern.search(sums):
                    print(
                        f"ERROR: {gomod.relative_to(root)}: missing go.sum entry for "
                        f"{module} {version}"
                    )
                    errors += 1

    for old, new in local_replaces:
        target = (gomod.parent / new).resolve()
        try:
            target.relative_to(root)
        except ValueError:
            print(
                f"ERROR: {gomod.relative_to(root)}: replace {old} -> {new} "
                "escapes the repository"
            )
            errors += 1
            continue
        if not target.exists():
            print(
                f"ERROR: {gomod.relative_to(root)}: replace {old} -> {new} "
                "does not exist"
            )
            errors += 1

    for old, old_version, new in replaces:
        if new.startswith("./") or new.startswith("../"):
            continue
        if new.startswith(("http://", "https://", "git@")):
            qualifier = f" {old_version}" if old_version else ""
            print(
                f"WARNING: {gomod.relative_to(root)}: remote replace "
                f"{old}{qualifier} -> {new}; "
                "verify hermetic prefetch can resolve it"
            )
            warnings += 1

print(
    f"Checked {checked} go.mod file(s); {errors} error(s), {warnings} warning(s)"
)
sys.exit(1 if errors else 0)
PY
)
  status=$?
  set -e
  printf '%s\n' "$output"

  if [ "$status" -ne 0 ]; then
    static_error "Go module validation found issues (see ERROR lines above)"
  else
    pass "Go module manifests and local replace targets are consistent"
  fi
}

check_python_manifests() {
  info "Checking async-upload Python requirements for hermetic build compatibility..."

  if [ ! -f jobs/async-upload/requirements.txt ]; then
    static_error "jobs/async-upload/requirements.txt is missing"
    return 0
  fi

  if ! command -v python3 >/dev/null 2>&1; then
    warn "python3 not available; skipping detailed requirements.txt validation"
    if grep -Eq 'git\+|https?://|file:|^-e ' jobs/async-upload/requirements.txt; then
      static_error "requirements.txt contains a non-hermetic dependency reference"
    else
      pass "requirements.txt has no obvious non-hermetic references"
    fi
    return 0
  fi

  local output status
  set +e
  output=$(python3 - <<'PY'
import pathlib
import re
import sys

requirements = pathlib.Path("jobs/async-upload/requirements.txt")
lock = pathlib.Path("jobs/async-upload/poetry.lock")
errors = 0
warnings = 0

if not lock.exists():
    print("ERROR: jobs/async-upload/poetry.lock is missing")
    errors += 1

lines = requirements.read_text().splitlines()
pins = {}
for number, line in enumerate(lines, 1):
    stripped = line.strip()
    if not stripped or stripped.startswith("#"):
        continue
    if stripped.startswith(("-e ", "--find-links", "--index-url", "--extra-index-url")):
        print(
            f"ERROR: {requirements}:{number}: unsupported hermetic pip option: {stripped}"
        )
        errors += 1
        continue
    if stripped.startswith(("git+", "http://", "https://", "file:")) or " @ " in stripped:
        print(
            f"ERROR: {requirements}:{number}: direct/VCS/local reference is not hermetic: "
            f"{stripped}"
        )
        errors += 1
        continue

    match = re.match(r"^([A-Za-z0-9_.-]+)==([^ ;]+)(?:\s*;.*)?$", stripped)
    if not match:
        print(
            f"ERROR: {requirements}:{number}: dependency is not exactly pinned: {stripped}"
        )
        errors += 1
        continue

    name = match.group(1).lower().replace("_", "-")
    version = match.group(2)
    if name in pins and pins[name] != version:
        print(
            f"ERROR: {requirements}:{number}: conflicting pins for {name}: "
            f"{pins[name]} and {version}"
        )
        errors += 1
    pins[name] = version

if lock.exists():
    lock_versions = {}
    for block in re.split(r"^\[\[package\]\]\s*$", lock.read_text(), flags=re.M)[1:]:
        name_match = re.search(r'^name = "([^"]+)"', block, re.M)
        version_match = re.search(r'^version = "([^"]+)"', block, re.M)
        if name_match and version_match:
            lock_versions[name_match.group(1).lower().replace("_", "-")] = (
                version_match.group(1)
            )

    for name, version in pins.items():
        if name not in lock_versions:
            print(f"ERROR: {requirements}: {name} is not present in poetry.lock")
            errors += 1
        elif lock_versions[name] != version:
            print(
                f"ERROR: {requirements}: {name}=={version} does not match "
                f"poetry.lock version {lock_versions[name]}"
            )
            errors += 1

print(f"Checked {len(pins)} pinned Python requirement(s); {errors} error(s), {warnings} warning(s)")
sys.exit(1 if errors else 0)
PY
)
  status=$?
  set -e
  printf '%s\n' "$output"

  if [ "$status" -ne 0 ]; then
    static_error "Python dependency validation found issues (see ERROR lines above)"
  else
    pass "requirements.txt is pinned and matches poetry.lock"
  fi
}

check_dockerfile_hygiene() {
  info "Checking Dockerfile hygiene and FIPS signals..."

  if grep -Eq 'go build|make build(/compile|/odh)?([[:space:]]|$)' Dockerfile; then
    if grep -q -- '-tags.*strictfipsruntime' Dockerfile; then
      pass "Dockerfile uses -tags strictfipsruntime"
    else
      warn "Dockerfile builds Go without -tags strictfipsruntime (RHOAI FIPS release blocker)"
    fi
    if grep -q 'CGO_ENABLED=0' Dockerfile; then
      warn "Dockerfile sets CGO_ENABLED=0; strict FIPS builds generally require CGO"
    fi
  fi

  if ! grep -q 'go mod download' Dockerfile; then
    warn "Dockerfile does not run 'go mod download' before building"
  fi

  while IFS= read -r from_line; do
    if printf '%s\n' "$from_line" | grep -Eq '^FROM .*:latest([[:space:]]|$)'; then
      warn "Dockerfile uses a floating :latest base image: $from_line"
    fi
  done < <(grep -E '^FROM ' Dockerfile || true)

  if ! grep -Eq '^USER[[:space:]]+[^0]' Dockerfile; then
    warn "Dockerfile does not declare a non-root USER"
  fi
  if ! grep -Eq '^USER[[:space:]]+[^0]' jobs/async-upload/Dockerfile; then
    warn "jobs/async-upload/Dockerfile does not declare a non-root USER"
  fi

  for dockerfile in Dockerfile jobs/async-upload/Dockerfile; do
    if grep -Eq 'microdnf (install|update)|dnf (install|update)' "$dockerfile"; then
      warn "$dockerfile installs RPMs; hermetic builds require RPM prefetching"
    fi
  done
}

check_go_replace_copies() {
  info "Checking that Dockerfile COPY covers local Go replace targets..."

  if ! command -v python3 >/dev/null 2>&1; then
    warn "python3 not available; skipping Dockerfile COPY/replace cross-check"
    return 0
  fi

  local output status
  set +e
  output=$(python3 - <<'PY'
import fnmatch
import json
import os
import pathlib
import re
import shlex
import sys

root = pathlib.Path(".").resolve()
gomod = pathlib.Path("go.mod")
dockerfile = pathlib.Path("Dockerfile")
errors = 0
checked = 0

if not gomod.exists() or not dockerfile.exists():
    print("ERROR: root go.mod or Dockerfile is missing")
    sys.exit(1)


def parse_replaces(text):
    replaces = []
    in_block = False
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("replace ("):
            in_block = True
            continue
        if in_block and stripped == ")":
            in_block = False
            continue
        if in_block:
            parts = stripped.split("=>")
            if len(parts) == 2:
                left = parts[0].strip().split()
                if left:
                    replaces.append((left[0], parts[1].strip()))
        elif stripped.startswith("replace "):
            parts = stripped.partition("=>")
            if parts[1]:
                left = parts[0].replace("replace", "", 1).strip().split()
                if left:
                    replaces.append((left[0], parts[2].strip()))
    return replaces


local_replaces = [
    (old, new)
    for old, new in parse_replaces(gomod.read_text())
    if new.startswith("./") or new.startswith("../")
]

copies = []
for line in dockerfile.read_text().splitlines():
    stripped = line.strip()
    if not stripped.upper().startswith("COPY "):
        continue
    rest = stripped[5:].strip()
    if rest.startswith("--from="):
        continue
    if rest.startswith("["):
        try:
            tokens = json.loads(rest)
        except json.JSONDecodeError:
            tokens = shlex.split(rest)
    else:
        tokens = shlex.split(rest)
    tokens = [token for token in tokens if not token.startswith("--")]
    if len(tokens) >= 2:
        copies.extend(tokens[:-1])


def covers(source, target):
    source = os.path.normpath(source)
    target = os.path.normpath(target)
    if source == ".":
        return True
    if source == target:
        return True
    if target.startswith(source.rstrip("/") + "/"):
        return True
    if "*" in source and fnmatch.fnmatch(target, source):
        return True
    return False


for old, new in local_replaces:
    target = os.path.normpath(new)
    checked += 1
    if not any(covers(source, target) for source in copies):
        print(
            f"ERROR: Dockerfile does not COPY local replace target {target} "
            f"(required by {old})"
        )
        errors += 1
    else:
        print(f"OK: Dockerfile COPY covers local replace target {target}")

print(f"Checked {checked} local replace target(s); {errors} error(s)")
sys.exit(1 if errors else 0)
PY
)
  status=$?
  set -e
  printf '%s\n' "$output"

  if [ "$status" -ne 0 ]; then
    static_error "Dockerfile COPY/replace validation failed (see ERROR lines above)"
  else
    pass "All local Go replace targets are present in the Docker build context"
  fi
}

validate_static() {
  STATIC_ISSUES=0
  check_konflux_targets
  check_go_manifests
  check_python_manifests
  check_dockerfile_hygiene
  check_go_replace_copies

  if [ "$STATIC_ISSUES" -gt 0 ]; then
    err "Static validation failed with $STATIC_ISSUES issue(s)"
    return 1
  fi
  pass "Static validation passed"
}

validate_go_modules() {
  if ! command -v go >/dev/null 2>&1; then
    warn "go not available; skipping go mod download/verify"
    return 0
  fi
  info "Downloading and verifying root Go modules..."
  # -mod=readonly makes a missing go.sum entry a hard failure instead of
  # silently rewriting go.sum in the CI checkout.
  GOFLAGS="-mod=readonly" go mod download
  GOFLAGS="-mod=readonly" go mod verify
  pass "Root Go modules downloaded and verified"
}

validate_hermetic() (
  local workdir
  workdir="$(mktemp -d)"
  trap 'chmod -R u+w "$workdir" 2>/dev/null || true; rm -rf "$workdir"' EXIT

  if command -v go >/dev/null 2>&1; then
    local modcache="$workdir/gomodcache"
    info "Populating an isolated Go module cache with network access..."
    GOMODCACHE="$modcache" GOFLAGS="-mod=readonly" go mod download
    pass "Go module cache populated"

    info "Compiling with GOPROXY=off (mirrors a Hermeto-prefetched build)..."
    GOMODCACHE="$modcache" GOPROXY=off GOFLAGS="-mod=readonly" \
      CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -o "$workdir/model-registry" .
    pass "Go build succeeds from the module cache alone"
  else
    warn "go not available; skipping hermetic Go build"
  fi

  local requirements="jobs/async-upload/requirements.txt"
  if command -v python3 >/dev/null 2>&1; then
    local wheelhouse="$workdir/wheelhouse"
    mkdir -p "$wheelhouse"
    info "Downloading async-upload distributions with network access..."
    python3 -m pip download --quiet --disable-pip-version-check \
      --only-binary=:all: -r "$requirements" -d "$wheelhouse" \
      || python3 -m pip download --quiet --disable-pip-version-check \
        -r "$requirements" -d "$wheelhouse"
    pass "Async-upload distributions downloaded to a local wheelhouse"

    # Resolve inside a venv so PEP 668 externally-managed environments
    # (Homebrew, Debian) do not block the dry-run install.
    if ! python3 -m venv "$workdir/venv" >/dev/null 2>&1; then
      warn "python3 venv support missing; skipping hermetic Python resolution"
      return 0
    fi
    local venv_python="$workdir/venv/bin/python"
    if ! "$venv_python" -m pip install --help 2>/dev/null | grep -q -- '--dry-run'; then
      warn "pip without --dry-run support; skipping hermetic Python resolution"
      return 0
    fi

    info "Resolving async-upload requirements with --no-index..."
    "$venv_python" -m pip install --dry-run --quiet --disable-pip-version-check \
      --no-index --find-links "$wheelhouse" -r "$requirements"
    pass "Async-upload requirements resolve from the wheelhouse alone"
  else
    warn "python3 not available; skipping hermetic Python resolution"
  fi
)

validate_manifests() {
  require_cmd "$KUSTOMIZE_BIN"

  local out_dir schema_cache=""
  out_dir=$(mktemp -d "${TMPDIR:-/tmp}/konflux-manifests.XXXXXX")
  local issues=0

  cleanup_manifest_tmp() {
    rm -rf "$out_dir" ${schema_cache:+"$schema_cache"}
  }

  info "Rendering kustomize overlays..."
  while IFS= read -r kust; do
    local dir name out
    dir=$(dirname "$kust")

    # This option directory is stale upstream: it references a file that no
    # longer exists and is not referenced by the deployment docs. Still check
    # it once the missing file is restored.
    if [ "$dir" = "manifests/kustomize/options/catalog/options/istio" ] && \
       [ ! -f "$dir/istio-authorization-policy.yaml" ]; then
      warn "Skipping known-stale kustomization $dir (missing istio-authorization-policy.yaml)"
      continue
    fi

    name=$(printf '%s' "$dir" | tr '/' '_')
    out="$out_dir/$name.yaml"
    if ! "$KUSTOMIZE_BIN" build "$dir" > "$out" 2>"$out_dir/$name.err"; then
      err "kustomize build failed for $dir"
      sed -n '1,20p' "$out_dir/$name.err" >&2
      issues=$((issues + 1))
      continue
    fi
    if ! grep -q '^kind:' "$out"; then
      err "kustomize build for $dir produced no Kubernetes resources"
      issues=$((issues + 1))
      continue
    fi
    pass "$dir"
  done < <(find manifests -name 'kustomization.y*ml' | sort)

  if [ "$issues" -gt 0 ]; then
    err "Kustomize validation failed with $issues issue(s)"
    cleanup_manifest_tmp
    return 1
  fi

  if command -v "$KUBECONFORM_BIN" >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
    info "Validating rendered resources with kubeconform ${KUBERNETES_VERSION}..."
    schema_cache=$(mktemp -d "${TMPDIR:-/tmp}/kubeconform-cache.XXXXXX")

    for out in "$out_dir"/*.yaml; do
      [ -f "$out" ] || continue
      local json="$out.json"
      local kc_err="$out.err"
      "$KUBECONFORM_BIN" -strict -ignore-missing-schemas \
        -kubernetes-version "$KUBERNETES_VERSION" -verbose \
        -cache "$schema_cache" -output json "$out" > "$json" 2>"$kc_err" || true

      if ! jq -e '.resources' "$json" >/dev/null 2>&1; then
        err "kubeconform produced no parseable output for $out"
        sed -n '1,20p' "$kc_err" >&2
        issues=$((issues + 1))
        continue
      fi

      local invalid
      invalid=$(jq -r '.resources[] | select(.status=="statusInvalid") | "\(.kind) \(.version): \(.msg)"' "$json" 2>/dev/null || true)
      if [ -n "$invalid" ]; then
        err "kubeconform found invalid resources in $out:"
        printf '%s\n' "$invalid" >&2
        issues=$((issues + 1))
      fi

      local errored
      errored=$(jq -r '.resources[] | select(.status=="statusError") | "\(.kind) \(.version): \(.msg)"' "$json" 2>/dev/null || true)
      if [ -n "$errored" ]; then
        err "kubeconform could not validate resources in $out:"
        printf '%s\n' "$errored" >&2
        issues=$((issues + 1))
      fi

      local bad_skip
      bad_skip=$(jq -r '
        .resources[] | select(.status=="statusSkipped")
        | . as $r
        | (if ($r.version|contains("/")) then ($r.version|split("/")[0]) else "" end) as $g
        | select($g=="" or ($g|endswith(".k8s.io")) or ($g|IN("apps","batch","policy","autoscaling","extensions")))
        | "  \($r.kind) (\($r.version))"' "$json" 2>/dev/null || true)
      if [ -n "$bad_skip" ]; then
        err "kubeconform skipped built-in kind(s) in $out — likely an apiVersion/kind typo:"
        printf '%s\n' "$bad_skip" >&2
        issues=$((issues + 1))
      fi

      local skipped
      skipped=$(jq -r '[.resources[] | select(.status=="statusSkipped") | .kind] | unique | join(", ")' "$json" 2>/dev/null || true)
      if [ -n "$skipped" ]; then
        info "  $(basename "$out"): schema-skipped CRD-backed kinds: $skipped"
      fi
    done

    if [ "$issues" -gt 0 ]; then
      err "Manifest schema validation failed with $issues issue(s)"
      cleanup_manifest_tmp
      return 1
    fi
    pass "All kustomize overlays rendered and passed schema checks"
  else
    warn "kubeconform or jq not available; skipped schema validation"
  fi

  cleanup_manifest_tmp
}

validate_server() (
  require_cmd docker
  require_cmd curl
  require_cmd jq
  validate_static || return 1

  info "Building server image $SERVER_IMAGE for $PLATFORM..."
  docker build --platform "$PLATFORM" --tag "$SERVER_IMAGE" -f Dockerfile .
  pass "Server image built"

  local suffix db_name server_name network_name db_password
  suffix="${GITHUB_RUN_ID:-$$}-$RANDOM"
  db_name="mr-validation-db-$suffix"
  server_name="mr-validation-server-$suffix"
  network_name="mr-validation-net-$suffix"
  if command -v openssl >/dev/null 2>&1; then
    db_password="$(openssl rand -hex 24)"
  else
    db_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  fi

  cleanup_server() {
    docker rm -f "$server_name" >/dev/null 2>&1 || true
    docker rm -f "$db_name" >/dev/null 2>&1 || true
    docker network rm "$network_name" >/dev/null 2>&1 || true
  }
  trap cleanup_server EXIT

  info "Starting Postgres ($POSTGRES_IMAGE)..."
  docker network create "$network_name" >/dev/null
  docker run -d --name "$db_name" --network "$network_name" \
    -e POSTGRES_DB=model_registry \
    -e POSTGRES_USER=postgres \
    -e "POSTGRES_PASSWORD=$db_password" \
    "$POSTGRES_IMAGE" >/dev/null

  local db_ready=false
  for _ in $(seq 1 30); do
    if docker exec "$db_name" pg_isready -U postgres -d model_registry >/dev/null 2>&1; then
      db_ready=true
      break
    fi
    sleep 2
  done
  if [ "$db_ready" != true ]; then
    err "Postgres did not become ready"
    docker logs "$db_name" || true
    return 1
  fi
  pass "Postgres ready"

  info "Starting model-registry server..."
  docker run -d --name "$server_name" --network "$network_name" \
    -p "$SERVER_PORT:8080" "$SERVER_IMAGE" proxy \
    --hostname=0.0.0.0 --port=8080 --datastore-type=embedmd \
    --embedmd-database-type=postgres \
    --embedmd-database-dsn="postgresql://postgres:${db_password}@${db_name}:5432/model_registry?sslmode=disable" >/dev/null

  local health_code=""
  for _ in $(seq 1 60); do
    health_code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${SERVER_PORT}/readyz/health" || true)
    [ "$health_code" = "200" ] && break
    sleep 2
  done
  if [ "$health_code" != "200" ]; then
    err "Server health check did not return 200 (last: ${health_code:-no response})"
    docker logs "$server_name" || true
    return 1
  fi
  pass "Health endpoint /readyz/health returned 200"

  local dirty_code
  dirty_code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${SERVER_PORT}/readyz/isDirty" || true)
  if [ "$dirty_code" != "200" ]; then
    err "/readyz/isDirty returned ${dirty_code:-no response}"
    docker logs "$server_name" || true
    return 1
  fi
  pass "Liveness endpoint /readyz/isDirty returned 200"

  info "Testing v1 API..."
  local api_response
  api_response=$(mktemp "${TMPDIR:-/tmp}/mr-registered-models.XXXXXX")
  if ! curl -fsS "http://127.0.0.1:${SERVER_PORT}/api/model_registry/v1/registered_models" > "$api_response"; then
    err "GET /api/model_registry/v1/registered_models failed"
    docker logs "$server_name" || true
    return 1
  fi
  if ! jq -e '.items | type == "array"' "$api_response" >/dev/null; then
    err "GET /api/model_registry/v1/registered_models returned an unexpected payload"
    cat "$api_response" || true
    return 1
  fi
  pass "GET /api/model_registry/v1/registered_models returned an items array"

  local uid
  uid=$(docker exec "$server_name" cat /proc/self/status 2>/dev/null | awk '/^Uid:/{print $2}' || true)
  if [ "$uid" != "65532" ]; then
    err "Server container is not running as UID 65532 (got ${uid:-unknown})"
    return 1
  fi
  pass "Server container runs as non-root UID 65532"

  sleep 10
  if [ "$(docker inspect -f '{{.State.Running}}' "$server_name" 2>/dev/null || echo false)" != "true" ]; then
    err "Server container exited during stability check"
    docker logs "$server_name" || true
    return 1
  fi
  pass "Server container stayed running"

  local logs
  logs=$(docker logs "$server_name" 2>&1 || true)
  if printf '%s\n' "$logs" | grep -q '{{ALERT}}'; then
    err "Server logs contain a datastore alert ({{ALERT}})"
    printf '%s\n' "$logs" | tail -50 >&2
    return 1
  fi
  if printf '%s\n' "$logs" | grep -Eqi 'panic:|fatal error|segmentation fault'; then
    err "Server logs contain a fatal error"
    printf '%s\n' "$logs" | tail -50 >&2
    return 1
  fi
  pass "Server logs contain no datastore or fatal errors"
  info "Last server log lines:"
  printf '%s\n' "$logs" | tail -20

  cleanup_server
  trap - EXIT
  pass "Server image validation passed"
)

validate_async_upload() {
  require_cmd docker
  validate_static || return 1

  info "Building async-upload image $ASYNC_IMAGE for $PLATFORM..."
  docker build --platform "$PLATFORM" --tag "$ASYNC_IMAGE" \
    -f jobs/async-upload/Dockerfile jobs/async-upload
  pass "Async-upload image built"

  info "Checking Python imports..."
  docker run --rm --entrypoint python "$ASYNC_IMAGE" -c \
    "import job, job.config, job.entrypoint, job.healthcheck, model_registry; print('imports ok')"
  pass "Python imports OK"

  info "Running pip dependency check..."
  docker run --rm --entrypoint python "$ASYNC_IMAGE" -m pip check
  pass "pip check passed"

  info "Running job healthcheck..."
  docker run --rm --entrypoint python "$ASYNC_IMAGE" -m job.healthcheck
  pass "job healthcheck exited 0"

  info "Checking cosign and skopeo..."
  docker run --rm --entrypoint cosign "$ASYNC_IMAGE" version
  docker run --rm --entrypoint skopeo "$ASYNC_IMAGE" --version
  pass "cosign and skopeo are available"

  local uid
  uid=$(docker run --rm --entrypoint cat "$ASYNC_IMAGE" /proc/self/status | awk '/^Uid:/{print $2}')
  if [ "$uid" != "1000" ]; then
    err "Async-upload image is not running as UID 1000 (got ${uid:-unknown})"
    return 1
  fi
  pass "Async-upload image runs as non-root UID 1000"

  local healthcheck
  healthcheck=$(docker inspect -f '{{json .Config.Healthcheck}}' "$ASYNC_IMAGE")
  if [ "$healthcheck" = "null" ] || [ -z "$healthcheck" ]; then
    warn "Async-upload image has no HEALTHCHECK"
  else
    pass "Async-upload image defines a HEALTHCHECK"
  fi

  pass "Async-upload image validation passed"
}

# Operator integration (Kind).
#
# Deploys opendatahub-io/model-registry-operator into Kind, points it at the PR
# image, and asserts that it reconciles a ModelRegistry resource. This is the
# layer the container checks cannot reach: CRD, RBAC, image wiring, and
# controller reconcile behaviour.
validate_operator() (
  require_cmd docker
  require_cmd kind
  require_cmd kubectl

  local cluster="${KIND_CLUSTER_NAME:-mr-operator-validation}"
  local node_image="${KIND_NODE_IMAGE:-kindest/node:v1.33.7}"
  local operator_repo="${OPERATOR_REPO:-https://github.com/opendatahub-io/model-registry-operator.git}"
  local operator_ref="${OPERATOR_REF:-c7cad77782f7dcec705111cee96c80a3b14c9cab}"
  local operator_ns="model-registry-operator-system"
  local operator_deploy="model-registry-operator-controller-manager"
  local registry_ns="${REGISTRY_NAMESPACE:-default}"
  local registry="modelregistry-sample"
  local timeout="${OPERATOR_TIMEOUT:-300s}"
  local context="kind-$cluster"
  local created_cluster=false

  if ! docker image inspect "$SERVER_IMAGE" >/dev/null 2>&1; then
    info "Server image $SERVER_IMAGE is not local; building it for $PLATFORM..."
    docker build --platform "$PLATFORM" --tag "$SERVER_IMAGE" -f Dockerfile .
  fi

  # Dump everything a controller failure needs, in the job that failed.
  dump_operator_state() {
    err "Operator integration did not converge; dumping cluster state"
    kubectl --context "$context" get deploy,pods -n "$operator_ns" -o wide || true
    kubectl --context "$context" describe deployment/"$operator_deploy" -n "$operator_ns" || true
    kubectl --context "$context" logs -n "$operator_ns" deployment/"$operator_deploy" --all-containers --tail=200 || true
    kubectl --context "$context" get all -n "$registry_ns" -o wide || true
    kubectl --context "$context" describe modelregistries.modelregistry.opendatahub.io/"$registry" -n "$registry_ns" || true
    kubectl --context "$context" get events -A --sort-by=.lastTimestamp | tail -40 || true
  }

  cleanup_operator() {
    if [ "${KEEP_KIND:-0}" = "1" ]; then
      info "Keeping Kind cluster $cluster (KEEP_KIND=1)"
      return
    fi
    if [ "$created_cluster" != "true" ]; then
      info "Leaving pre-existing Kind cluster $cluster in place"
      return
    fi
    kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
  }
  trap cleanup_operator EXIT

  if kind get clusters 2>/dev/null | grep -qx "$cluster"; then
    info "Reusing existing Kind cluster $cluster"
  else
    info "Creating Kind cluster $cluster ($node_image)..."
    kind create cluster --name "$cluster" --image "$node_image" --wait 300s
    created_cluster=true
  fi

  info "Loading the PR image into Kind..."
  kind load docker-image "$SERVER_IMAGE" --name "$cluster"
  pass "PR image is available in the cluster"

  info "Deploying model-registry-operator at ${operator_ref:0:12}..."
  kubectl --context "$context" apply -k "${operator_repo}/config/default?ref=${operator_ref}"

  # The operator resolves workload images from RELATED_IMAGE_* variables, not
  # from the Deployment spec, so this is what actually tests the PR image.
  info "Pointing the operator at the PR image..."
  kubectl --context "$context" -n "$operator_ns" set env deployment/"$operator_deploy" \
    "RELATED_IMAGE_ODH_MODEL_REGISTRY_IMAGE=$SERVER_IMAGE" >/dev/null

  if ! kubectl --context "$context" -n "$operator_ns" rollout status deployment/"$operator_deploy" --timeout="$timeout"; then
    dump_operator_state
    return 1
  fi
  pass "Operator is Available"

  info "Creating the MySQL-backed ModelRegistry sample..."
  kubectl --context "$context" apply -k "${operator_repo}/config/samples/mysql?ref=${operator_ref}" >/dev/null

  if ! kubectl --context "$context" -n "$registry_ns" wait --for=condition=Available deployment/model-registry-db --timeout="$timeout"; then
    dump_operator_state
    return 1
  fi
  pass "Sample database is Available"

  if ! kubectl --context "$context" -n "$registry_ns" wait --for=condition=Available \
    modelregistries.modelregistry.opendatahub.io/"$registry" --timeout="$timeout"; then
    dump_operator_state
    return 1
  fi
  pass "ModelRegistry reconciled to Available"

  local finalizers
  finalizers=$(kubectl --context "$context" -n "$registry_ns" get modelregistries.modelregistry.opendatahub.io/"$registry" \
    -o jsonpath='{.metadata.finalizers[*]}')
  if [[ " $finalizers " != *" modelregistry.opendatahub.io/finalizer "* ]]; then
    err "Expected modelregistry.opendatahub.io/finalizer (got: ${finalizers:-none})"
    dump_operator_state
    return 1
  fi
  pass "Reconcile finalizer is set"

  # The Deployment is named after the CR, so select it by the label the operator
  # template sets instead of guessing the name.
  local deployed_image
  deployed_image=$(kubectl --context "$context" -n "$registry_ns" get deployment -l component=model-registry \
    -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null || true)
  if [ -z "$deployed_image" ]; then
    err "Operator did not create a model-registry Deployment in namespace $registry_ns"
    dump_operator_state
    return 1
  fi
  if [ "$deployed_image" != "$SERVER_IMAGE" ]; then
    err "Operator deployed '${deployed_image}' instead of the PR image '$SERVER_IMAGE'"
    dump_operator_state
    return 1
  fi
  pass "Operator rendered the PR image"

  # A reused cluster (KEEP_KIND=1) keeps the same tag, so `kind load` swaps the
  # image in the node's store while the Deployment's pod template is unchanged
  # and the running containers still hold the old content. Restarting the
  # Deployment is what makes the loaded image take effect -- and it also bumps
  # the generation, so the rollout wait below has something real to wait for.
  # Deleting the pods instead would leave both waits satisfied by the old,
  # already-Available Deployment.
  if ! kubectl --context "$context" -n "$registry_ns" rollout restart deployment \
    -l component=model-registry; then
    dump_operator_state
    return 1
  fi

  if ! kubectl --context "$context" -n "$registry_ns" wait --for=condition=Available deployment \
    -l component=model-registry --timeout="$timeout"; then
    dump_operator_state
    return 1
  fi
  pass "Model Registry deployment is Available"

  # Available can be satisfied while the operator is still replacing the
  # workload: it renders :latest first and switches to the PR image once the
  # RELATED_IMAGE_* change reaches it, which rolls the Deployment over. Wait for
  # that rollout to finish before snapshotting, or the terminating pod from the
  # superseded ReplicaSet reads as instability.
  #
  # The operator renders :latest until the RELATED_IMAGE_* patch rolls out, and
  # the server can start before MySQL accepts connections, so a restart during
  # startup is expected. What matters is that it is not still restarting once
  # the rollover has completed.
  if ! kubectl --context "$context" -n "$registry_ns" rollout status deployment \
    -l component=model-registry --timeout="$timeout"; then
    dump_operator_state
    return 1
  fi
  pass "Model Registry rollout completed"

  # Terminating pods belong to the previous ReplicaSet and are expected to
  # vanish; including them would report a rollover as a restart or a
  # disappearance.
  #
  # Counts are keyed by pod UID and container name: summing across pods hides a
  # restart whenever a replacement pod has a lower count, and an empty pod list
  # must not read as "no restarts".
  restart_snapshot() {
    kubectl --context "$context" -n "$registry_ns" get pods -l component=model-registry \
      -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{$uid := .metadata.uid}}{{range .status.containerStatuses}}{{$uid}} {{.name}} {{.restartCount}}{{"\n"}}{{end}}{{end}}{{end}}' \
      2>/dev/null
  }

  local restarts_before restarts_after settle="${OPERATOR_SETTLE_SECONDS:-30}"
  restarts_before=$(restart_snapshot)
  if [ -z "$restarts_before" ]; then
    err "No model-registry pods found in namespace $registry_ns"
    dump_operator_state
    return 1
  fi
  info "Waiting ${settle}s to confirm the deployment is stable..."
  sleep "$settle"
  restarts_after=$(restart_snapshot)
  if [ -z "$restarts_after" ]; then
    err "No model-registry pods found in namespace $registry_ns after the settle window"
    dump_operator_state
    return 1
  fi

  local restart_regression
  restart_regression=$(
    awk '
      NR == FNR { before[$1 SUBSEP $2] = $3; next }
      {
        seen[$1 SUBSEP $2] = 1
        if (!(($1 SUBSEP $2) in before)) {
          printf "pod %s container %s appeared after Available\n", $1, $2
        } else if ($3 + 0 > before[$1 SUBSEP $2] + 0) {
          printf "pod %s container %s restarted %d time(s)\n", $1, $2, $3 - before[$1 SUBSEP $2]
        }
      }
      END {
        for (key in before) {
          if (!(key in seen)) {
            split(key, parts, SUBSEP)
            printf "pod %s container %s disappeared\n", parts[1], parts[2]
          }
        }
      }
    ' <(printf '%s\n' "$restarts_before") <(printf '%s\n' "$restarts_after")
  )
  if [ -n "$restart_regression" ]; then
    err "Model Registry changed after becoming Available:"
    printf '%s\n' "$restart_regression"
    kubectl --context "$context" -n "$registry_ns" logs -l component=model-registry --previous --tail=100 || true
    dump_operator_state
    return 1
  fi
  if printf '%s\n' "$restarts_before" | awk '$3 > 0 { found = 1 } END { exit found ? 0 : 1 }'; then
    warn "Model Registry restarted during startup, before it became Available"
    kubectl --context "$context" -n "$registry_ns" logs -l component=model-registry --previous --tail=50 || true
  fi
  pass "Model Registry pods are stable after reaching Available"

  pass "Operator integration validation passed"
)

case "$MODE" in
  static)
    validate_static
    ;;
  go-modules)
    validate_go_modules
    ;;
  hermetic)
    validate_hermetic
    ;;
  manifests)
    validate_manifests
    ;;
  server)
    validate_server
    ;;
  async-upload)
    validate_async_upload
    ;;
  operator)
    validate_operator
    ;;
  all)
    validate_static
    validate_go_modules
    validate_hermetic
    validate_manifests
    validate_server
    validate_async_upload
    validate_operator
    ;;
  *)
    echo "Usage: $0 <static|go-modules|hermetic|manifests|server|async-upload|operator|all>" >&2
    exit 2
    ;;
esac
