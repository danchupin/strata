# PRD: Supply-Chain Security (Cycle C — Prod-Readiness)

## Introduction

Cycle C of the 2026-05-25 prod-readiness audit closes the supply-chain trust gap. Strata today ships zero CVE scanners in CI, no dependency-update automation, no security disclosure policy, no published Docker image at `ghcr.io/danchupin/strata`, and no image signing or provenance — meaning a security-conscious operator vetting Strata has no way to verify the binary was built from the published source, no signal that deps are kept current, no channel to report vulnerabilities, and no automated check that the image they pull is CVE-free.

This cycle delivers:

1. **Three CVE / static-analysis scanners in CI** as parallel jobs — `govulncheck` (Go-native), `trivy` (container CVEs), `gosec` (Go static security analyzer).
2. **Dependabot dependency-update automation** — weekly cadence for Go × 2 modules + Actions + npm + Docker base images; patch-only auto-merge; grouped per ecosystem.
3. **`SECURITY.md` + disclosure policy** at repo root, GitHub Security Advisories (GHSA) primary disclosure channel (no email — GHSA is encrypted + audit-trailed; one channel keeps the contract clean).
4. **First release tag + image push + supply-chain provenance** — cut `v0.0.1-alpha.1`, push the first image to `ghcr.io/danchupin/strata` via the `slsa-framework/slsa-github-generator` reusable workflow (builds + pushes + attests in one verified pass for SLSA L3), emit SPDX SBOM via syft, sign via cosign keyless OIDC, document operator verify recipe at `/operate/image-verification.md` with k8s admission control examples. License audit via `go-licenses` against the SBOM gates banned licenses (GPL, AGPL).

After this cycle a security-conscious operator can: (a) verify Strata `main` is CVE-free at merge time, (b) verify the binary image they pull is signed by GitHub Actions identity, (c) feed SBOM into internal supply-chain tooling, (d) understand maintenance posture (weekly Dependabot auto-patch), (e) report vulnerabilities via GHSA. No code-side behavior changes — every gate is CI / release-engineering only.

## User Journey

A security-conscious operator vetting Strata for adoption:

1. **Read `SECURITY.md`** at repo root. Finds: supported versions table (`latest` + last 2 minor tags since pre-launch), disclosure channel (GitHub Security Advisories — Security tab on the repo), 90-day disclosure SLA, embargo policy, in-scope + out-of-scope clarifications, Hall of Fame section.
2. **Read `.github/workflows/ci.yml`** (or browse Actions tab). Sees 3 parallel scanner jobs running on every PR — `govulncheck`, `trivy-image`, `gosec` — each gated by appropriate severity threshold + uploading SARIF to GitHub Code Scanning. Confidence: Strata main is CVE-free at merge time.
3. **Pull `ghcr.io/danchupin/strata:v0.0.1-alpha.1`** (or `:latest`). Reads `/operate/image-verification.md`. Runs:
   ```
   cosign verify \
     --certificate-identity-regexp='^https://github\.com/danchupin/strata/.*' \
     --certificate-oidc-issuer-regexp='^https://token\.actions\.githubusercontent\.com$' \
     ghcr.io/danchupin/strata:v0.0.1-alpha.1
   ```
   cosign confirms image signed by GitHub Actions OIDC identity from the expected repo. Operator pins this regexp pair into their k8s admission controller (Kyverno / Gatekeeper / Sigstore Policy).
4. **Download SBOM artifact** from latest GitHub Release (or via `gh release download v0.0.1-alpha.1 -p '*sbom*'`). SPDX 2.3 JSON. Feeds into internal Trivy / Grype / Dependency-Track. Confirms: no banned packages, no license violations (already pre-checked by Cycle C `go-licenses` gate), no known-vulnerable transitive deps.
5. **Review `.github/dependabot.yml`**. Sees weekly cadence + grouped per-ecosystem + patch-only auto-merge. Understands: patches auto-land on Friday; majors/minors require human review.

## Goals

- Ship 3 CVE / static-analysis scanners as parallel CI jobs gated by appropriate severity thresholds.
- Ship Dependabot config covering all 5 ecosystem entries (Go × 2, Actions, npm, Docker × 2 dirs) with grouping.
- Ship `SECURITY.md` with GHSA-primary disclosure policy.
- Cut first release tag `v0.0.1-alpha.1` + push first image to `ghcr.io/danchupin/strata` via SLSA generator reusable workflow.
- SLSA L3 build provenance + SPDX SBOM + cosign keyless signing on every published image.
- License audit (banned-license gate) via `go-licenses` against SBOM.
- `/operate/image-verification.md` operator-facing verify recipe with k8s admission control examples.
- No regression on existing smoke / CI / s3-tests / e2e suites.

## User Stories

### US-001: govulncheck CI integration as parallel job + baseline scan
**Description:** As a maintainer, I want every PR to run `govulncheck` against the Go callgraph as a parallel CI job so CVE-affected code paths in Strata's own callgraph block merge without blowing the lint-build runtime budget.

**Acceptance Criteria:**
- [ ] New job `govulncheck` in `.github/workflows/ci.yml` as a **parallel job** (sibling of existing `lint-build` / `unit`, `needs: [web-build]` only — runs independently). NOT serial extension of `lint-build` (would push that job from ~3min to ~7-8min — too tight).
- [ ] Job installs govulncheck via `go install golang.org/x/vuln/cmd/govulncheck@latest` + runs `govulncheck -format json ./...` against main module.
- [ ] **Matrix axis**: separately runs against `internal/data/rados/cephimpl/` module via `cd internal/data/rados/cephimpl && GOWORK=off govulncheck -format json ./...` (per project CLAUDE.md cephimpl module split discipline).
- [ ] **Severity gate via jq parsing**: hard-fail (exit 1) when ANY finding has severity HIGH or CRITICAL **AND** affects code in `internal/` or `cmd/` callgraph. Transitive-only or test-only callgraph matches log as WARN + continue. Implemented in `scripts/check-govulncheck.sh` using `jq` (already on ubuntu-latest runner — verify presence + install fallback `apt-get install -y jq`).
- [ ] Severity classification helpers extracted to `scripts/lib/severity.sh` — reusable across future scanners (US-002 trivy, US-003 gosec, US-009 go-licenses).
- [ ] **Baseline scan at impl time**: run locally + remediate any HIGH/CRITICAL finding via dep upgrade BEFORE this PR merges. Document each remediated CVE in commit message.
- [ ] **SHA-pin all third-party actions** — at impl time, fetch latest stable release SHA for each action via `gh api repos/<owner>/<repo>/releases/latest` and pin via `<action>@<sha>` syntax. Document each pinned SHA + version in commit message.
- [ ] New `make govulncheck` Makefile target wraps `scripts/check-govulncheck.sh` (degrades to WARN + exit 0 when govulncheck binary missing — matches `make helm-lint` pattern).
- [ ] ROADMAP entry CREATED in `ROADMAP.md` under `## Correctness & consistency` section: `- **P0/P1 — Cycle C: supply-chain-security (govulncheck + trivy + gosec CI gates + Dependabot + SECURITY.md + first release tag + SLSA L3 provenance + SPDX SBOM + cosign signing + license audit).** In progress on ralph/supply-chain-security. Closes 4 supply-chain gaps from 2026-05-25 audit. Flipped Done on cycle close (US-011).`
- [ ] Typecheck passes
- [ ] Tests pass

### US-002: trivy CI integration as parallel job on docker-build artifact
**Description:** As a maintainer, I want every PR to scan the built Docker image for CVEs as a parallel CI job so OS-level + base-image vulnerabilities surface at merge time without inflating wall-clock.

**Acceptance Criteria:**
- [ ] New job `trivy-image` in `.github/workflows/ci.yml` as a **parallel job** (`needs: [docker-build]` only — runs in parallel with `e2e` / `e2e-full` which also depend on `docker-build`).
- [ ] Job uses `aquasecurity/trivy-action@<sha>` SHA-pinned (fetch latest stable release SHA at impl time + document in commit message).
- [ ] Scan target: locally-built image `strata:ceph` from `docker-build` job (loaded as artifact via `docker save | docker load` from a buildx cache, OR re-built in this job — choose faster path).
- [ ] **Severity gate**: hard-fail (exit 1) on CRITICAL CVE; emit WARN + exit 0 on HIGH-only. Use action's `severity: 'CRITICAL'` + `exit-code: 1` inputs. Separate WARN pass runs `severity: 'HIGH'` + `exit-code: 0` and uploads JSON output as workflow artifact for review.
- [ ] **Configurable via `trivy.yaml`** in repo root — operators can extend per-release-branch policy; this cycle establishes baseline.
- [ ] **Baseline scan at impl time**: build current `strata:ceph` locally + `trivy image --severity CRITICAL strata:ceph`. If any CRITICAL hit, document remediation in commit message (likely `dnf upgrade` line in Dockerfile OR base image minor bump from `quay.io/ceph/ceph:v19.2.3` to a newer minor — distroless migration is Cycle A2 out of scope).
- [ ] **Workflow permission**: `security-events: write` (required for SARIF upload) explicit in job permissions block.
- [ ] Trivy SARIF output uploaded to GitHub Code Scanning via `github/codeql-action/upload-sarif@<sha>` SHA-pinned action so findings appear in repo Security tab.
- [ ] New `make trivy-check` Makefile target wraps `trivy image --severity CRITICAL strata:ceph` (degrades to WARN + exit 0 when trivy binary missing).
- [ ] Typecheck passes
- [ ] Tests pass

### US-003: gosec CI integration as parallel job + baseline scan
**Description:** As a maintainer, I want every PR to run `gosec` against Go source as a parallel CI job so anti-patterns surface at merge time.

**Acceptance Criteria:**
- [ ] New job `gosec` in `.github/workflows/ci.yml` as a **parallel job** (sibling of `govulncheck`, `needs: [web-build]`).
- [ ] Job uses `securego/gosec/v2@<sha>` SHA-pinned action (fetch + document SHA at impl time).
- [ ] **Severity gate**: hard-fail (exit 1) on MEDIUM+ findings (`-severity=medium` action input). Skip test files via `-exclude-dir=*test*` OR via per-line `// nosec G<n>: <reason>` annotations.
- [ ] **Config file** `.gosec.yml` in repo root — explicit rule excludes documented per-rule with rationale. Initial excludes (after baseline scan): `G104` (unhandled errors — too noisy without `errcheck` integration; deferred), `G304` (file path provided as taint input — Strata reads cert/CA files at boot, expected pattern).
- [ ] **Baseline scan at impl time**: run `gosec -severity=medium ./...` locally. Result: ≤5 new `// nosec G<n>: <reason>` annotations added; if more, scope expanded and remediation preferred over annotation. Document each annotation + rationale in commit message.
- [ ] **Workflow permission**: `security-events: write` explicit in job permissions block.
- [ ] gosec SARIF output uploaded to GitHub Code Scanning via `github/codeql-action/upload-sarif@<sha>`.
- [ ] New `make gosec` Makefile target wraps `gosec -severity=medium ./...` (degrades to WARN + exit 0 when gosec binary missing).
- [ ] Typecheck passes
- [ ] Tests pass

### US-004: Dependabot config (5 ecosystems, grouped, patch-only auto-merge)
**Description:** As a maintainer, I want Dependabot to open weekly PRs for outdated dependencies — grouped to avoid PR firehose — so security patches land without manual tracking.

**Acceptance Criteria:**
- [ ] New file `.github/dependabot.yml` covering 5 ecosystem entries:
  - **Go main module** (`/go.mod`): weekly schedule (Friday 06:00 UTC), label `dependencies` + `go`, grouped:
    ```yaml
    groups:
      go-prod-patch:
        patterns: ["*"]
        update-types: ["patch"]
      go-prod-minor-major:
        patterns: ["*"]
        update-types: ["minor", "major"]
    ```
  - **Go cephimpl module** (`/internal/data/rados/cephimpl/go.mod`): same schedule + grouping, label `dependencies` + `go` + `cephimpl`.
  - **GitHub Actions** (`/`): weekly schedule (Friday 06:00 UTC), label `dependencies` + `actions`, grouped single `actions-all` group covering all updates (Actions versions infrequent + low-risk).
  - **npm** (`/web/package.json`): weekly schedule, label `dependencies` + `npm`, grouped by `npm-prod-patch` (patch-only) + `npm-prod-minor-major` (minor + major).
  - **Docker base images** — TWO entries: `/deploy/docker/` + `/deploy/docker/ceph-bootstrap/`. Weekly schedule, label `dependencies` + `docker`. No grouping (one Dockerfile = one base image; PR per ecosystem).
- [ ] **`open-pull-requests-limit: 5`** per ecosystem to avoid PR queue overflow when a security advisory hits.
- [ ] **Commit message convention**: `chore(deps): <ecosystem> <package> <old> → <new>` via Dependabot's `commit-message: prefix: chore(deps)` config.
- [ ] **Auto-merge policy (patch-only)** wired via new workflow `.github/workflows/dependabot-auto-merge.yml`:
  - Triggers on `pull_request_target` from `dependabot[bot]`.
  - Uses `dependabot/fetch-metadata@<sha>` SHA-pinned action to extract `update-type`.
  - IF `update-type == 'version-update:semver-patch'` AND CI green → `gh pr merge --auto --squash`.
  - Major + minor updates leave PR open for human review (no auto-merge).
- [ ] Document auto-merge policy in `SECURITY.md` (US-005) so security researchers know patch-class advisories ship within 1 week.
- [ ] Verify Dependabot config validity via `gh api -X POST /repos/<owner>/<repo>/dependabot/alerts` or by enabling Dependabot in repo Settings + checking the alerts UI loads without parse error.
- [ ] Typecheck passes
- [ ] Tests pass

### US-005: SECURITY.md + GHSA-only disclosure policy
**Description:** As a security researcher, I want a published disclosure channel + supported-versions policy so I can report vulnerabilities responsibly.

**Acceptance Criteria:**
- [ ] New file `SECURITY.md` at repo root.
- [ ] **Supported versions table**:
  ```
  | Version | Supported |
  |---------|-----------|
  | latest (main) | :white_check_mark: |
  | tagged releases | :white_check_mark: (latest 2 minor versions) |
  | older tags | :x: |
  ```
- [ ] **Disclosure channel: GitHub Security Advisories ONLY** (no email backup). Link to repo's Security tab disclosure form (`https://github.com/danchupin/strata/security/advisories/new`). Rationale: GHSA encrypted + audit-trailed + zero-setup; email channel adds SPF/DKIM/maintainer-mailbox-rotation overhead with no security gain. Single channel = clean contract.
- [ ] **SLA**: 5 business days to acknowledge; 90-day disclosure deadline (ship fix OR downgrade severity); coordinated disclosure after fix lands.
- [ ] **Embargo policy**: researcher agrees to keep vulnerability private until fix released.
- [ ] **Out-of-scope** clarifications: self-hosted misconfigurations (operator runs Strata with `STRATA_AUTH_MODE=disabled` in prod), network-layer attacks (TLS termination is operator's responsibility unless using Cycle A's built-in TLS), denial-of-service via legitimate-but-expensive requests (use Cycle A rate limiter).
- [ ] **In-scope** examples: SigV4 verifier bypass, IAM policy evaluator bypass, audit-log forge, KMS DEK leak, manifest tampering, CRUD method privilege escalation, panic-on-malformed-input (DoS-by-crash).
- [ ] Cross-link to `/best-practices/production-hardening.md` from Cycle A.
- [ ] **Hall of fame** / acknowledgements section — initially empty; populated as researchers report valid issues.
- [ ] **Patch-class advisory cadence**: document that Dependabot patch-class auto-merge (US-004) means downstream patches land within 1 week of upstream advisory.
- [ ] `README.md` extended with one-line `## Security` section pointing at `SECURITY.md`.
- [ ] Typecheck passes
- [ ] Tests pass

### US-006: First release tag + image push via SLSA generator reusable workflow
**Description:** As a maintainer, I want to cut the first release tag and push the inaugural image to `ghcr.io/danchupin/strata` via the `slsa-framework/slsa-github-generator` reusable workflow — which orchestrates Docker build + GHCR push + SLSA L3 provenance attest in one verified pass — so subsequent stories (SBOM, cosign sign, verify recipe) have a real published image to attach to.

**Acceptance Criteria:**
- [ ] New workflow `.github/workflows/release-image.yml` triggered on `push: tags: ['v*']` + `workflow_dispatch` (manual trigger for non-tag rebuilds).
- [ ] **Workflow uses `slsa-framework/slsa-github-generator/.github/workflows/generator_container_slsa3.yml@<sha>` reusable workflow** — SHA-pinned (fetch latest v2.x release SHA at impl time). The SLSA generator IS the build orchestrator: it builds the Docker image, pushes to GHCR, AND generates non-falsifiable provenance attestation in one verified pass. **Do NOT split build + push + attest into separate steps** — SLSA L3 requires the generator to own the entire build pipeline.
- [ ] Generator inputs configured for Strata: `image: ghcr.io/danchupin/strata`, `registry-username: ${{ github.actor }}`, `registry-password: ${{ secrets.GITHUB_TOKEN }}`, Dockerfile path = `deploy/docker/Dockerfile`, build context = repo root.
- [ ] **Workflow permissions** explicit + minimal at top of file: `contents: read`, `packages: write` (GHCR push), `id-token: write` (OIDC for SLSA + cosign), `attestations: write`.
- [ ] **Cut first tag `v0.0.1-alpha.1`** as part of this US — pre-launch SemVer-pre-release shape. Tagging shape: `git tag -a v0.0.1-alpha.1 -m 'Cycle C first release: supply-chain-security baseline'` + `git push origin v0.0.1-alpha.1`. Triggers the new release-image.yml workflow end-to-end.
- [ ] Verify the released image appears at `ghcr.io/danchupin/strata:v0.0.1-alpha.1` + `:latest` (tag the workflow promotes when on a `v*` tag).
- [ ] **Workflow output**: image digest exposed as workflow output for downstream stories (US-007 SBOM, US-008 cosign sign) to consume — guarantees same-digest signing across all attestations.
- [ ] **Provenance verify recipe** (operator-facing, documented in US-010 `/operate/image-verification.md`):
  ```
  cosign verify-attestation \
    --type slsaprovenance \
    --certificate-identity-regexp='^https://github\.com/danchupin/strata/.*' \
    --certificate-oidc-issuer-regexp='^https://token\.actions\.githubusercontent\.com$' \
    ghcr.io/danchupin/strata:v0.0.1-alpha.1
  ```
- [ ] Typecheck passes
- [ ] Tests pass

### US-007: SPDX SBOM emission via syft (consumes US-006 digest)
**Description:** As a security-conscious operator, I want SPDX SBOM attached to every Strata release so I can feed it into supply-chain audit tooling.

**Acceptance Criteria:**
- [ ] Extend `.github/workflows/release-image.yml` from US-006 with an `sbom` job that **runs AFTER `slsa-framework/slsa-github-generator` reusable workflow completes** (`needs: [build-slsa]`).
- [ ] Job consumes the **image digest from US-006 workflow output** — ensures same-digest SBOM (no race where syft scans a different image than was attested).
- [ ] Job uses `anchore/sbom-action@<sha>` SHA-pinned action — SBOM via syft for accurate Go module + OS package detection.
- [ ] **Format**: SPDX 2.3 JSON (`format: spdx-json`). Linux Foundation standard.
- [ ] SBOM file `strata-<tag>-sbom.spdx.json`:
  - Uploaded as workflow artifact (90-day retention).
  - Attached to the matching GitHub Release via `softprops/action-gh-release@<sha>` SHA-pinned action.
- [ ] **Verify recipe** (operator-facing, documented in US-010):
  ```
  gh release download v<tag> -p '*sbom*' -R danchupin/strata
  grype sbom:strata-<tag>-sbom.spdx.json
  # OR: trivy sbom strata-<tag>-sbom.spdx.json
  ```
- [ ] Re-run smoke against the published image from US-006 to verify SBOM is fetchable + parseable.
- [ ] Typecheck passes
- [ ] Tests pass

### US-008: cosign keyless signing (consumes US-006 digest) + verify recipe
**Description:** As a security-conscious operator, I want every Strata image signed by the GitHub Actions OIDC identity so I can verify provenance via cosign without trusting any maintainer-held private key.

**Acceptance Criteria:**
- [ ] Extend `.github/workflows/release-image.yml` from US-006 with a `cosign-sign` job that **runs AFTER `slsa-framework/slsa-github-generator` reusable workflow completes** (`needs: [build-slsa]`).
- [ ] Job consumes the **image digest from US-006 workflow output** — ensures cosign signs the same digest that SLSA attested + SBOM scanned.
- [ ] Job uses `sigstore/cosign-installer@<sha>` SHA-pinned to install cosign + runs `cosign sign --yes ghcr.io/danchupin/strata@<digest>`.
- [ ] **Keyless signing via OIDC** — no static signing key. GitHub Actions OIDC identity becomes the signer; signature verifiable via Fulcio (public-good signing infrastructure) + Rekor transparency log.
- [ ] **Workflow permission** `id-token: write` (already declared in US-006) is what makes keyless OIDC work.
- [ ] **Operator verify recipe** (documented in US-010 `/operate/image-verification.md`) — anchored, escaped, suffix-matched regexps:
  ```
  cosign verify \
    --certificate-identity-regexp='^https://github\.com/danchupin/strata/.*' \
    --certificate-oidc-issuer-regexp='^https://token\.actions\.githubusercontent\.com$' \
    ghcr.io/danchupin/strata:v0.0.1-alpha.1
  ```
  Regexp pair pinned in `/operate/image-verification.md` — copy-paste-runnable.
- [ ] Implementation-time smoke (NOT requiring published image): `scripts/smoke-image-verification.sh` runs against a locally-built + locally-signed image via `cosign sign --key cosign.key` for dev-mode reproducibility (no GHCR push required for smoke; smoke is the recipe shape verification, not the published-signature check — the published-signature check is the operator's job post-deploy).
- [ ] Typecheck passes
- [ ] Tests pass

### US-009: License audit via go-licenses + SBOM gate
**Description:** As a maintainer, I want banned licenses (GPL, AGPL, LGPL strong copyleft, restricted commercial) to fail CI so accidental import of incompatible deps blocks merge.

**Acceptance Criteria:**
- [ ] New job `license-audit` in `.github/workflows/ci.yml` as a **parallel job** (`needs: [web-build]`).
- [ ] Job installs `go-licenses` via `go install github.com/google/go-licenses@<sha>` SHA-pinned + runs `go-licenses check ./... --disallowed_types=forbidden,restricted` against main module.
- [ ] **Banned licenses** (forbidden + restricted classes per Google's go-licenses taxonomy): GPL-2.0, GPL-3.0, AGPL-3.0, LGPL-2.1, LGPL-3.0, CDDL-1.0, EPL-1.0, EPL-2.0, MPL-2.0 (case-by-case — flag for review), commercial proprietary.
- [ ] **Allowed licenses**: Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC, Unlicense.
- [ ] **Baseline scan at impl time**: run `go-licenses check ./...` locally + cephimpl module. If any banned license found, either: (a) remediate via dep removal/replacement, OR (b) document an exception in `.licensei.yml` config file (one-off per-package allowlist with rationale).
- [ ] License report emitted via `go-licenses report ./... > license-report.csv` + uploaded as workflow artifact (operator-facing license inventory).
- [ ] **SBOM cross-check**: separate workflow step (US-009 OR US-007 — wire here) re-runs `go-licenses check` against the SBOM extracted in US-007. Double-check via two independent code-paths.
- [ ] New `make license-audit` Makefile target wraps `go-licenses check ./...` (degrades to WARN + exit 0 when go-licenses binary missing).
- [ ] Typecheck passes
- [ ] Tests pass

### US-010: /operate/image-verification.md + k8s admission control examples
**Description:** As a security-conscious operator, I want a published verify recipe + k8s admission control examples so I can gate deploys on cosign signature validity.

**Acceptance Criteria:**
- [ ] New doc page `docs/site/content/operate/image-verification.md`:
  - **What signing buys you** section (1 paragraph): provenance trail from source → CI → image; tamper-proof; no maintainer key to leak; Rekor transparency log for forensic audit.
  - **Verify recipe** — full chain operators run before deploying:
    - `cosign verify` (US-008 regexp — anchored, escaped, suffix-matched).
    - `cosign verify-attestation --type slsaprovenance ...` (US-006 SLSA provenance).
    - `grype sbom:strata-<tag>-sbom.spdx.json` (US-007 SBOM scan).
    - `go-licenses report` against SBOM (US-009 — verify no banned licenses).
  - **Rekor transparency log query**: how to query Rekor directly for forensic use (`rekor-cli search --sha <digest>`).
  - **k8s admission control** section: example Kyverno ClusterPolicy + Sigstore Policy CR manifests gating Strata image deploys on cosign signature validity. Reference manifests committed under `deploy/k8s/admission-controllers/`.
  - **Troubleshooting** section: common errors (`certificate verification failed`, `no matching attestations`, `image not signed`, `Rekor entry not found`) with fix recipes.
- [ ] New directory `deploy/k8s/admission-controllers/` with two example manifests:
  - `kyverno-strata-image-signed.yaml` — Kyverno ClusterPolicy verifying images by issuer + identity regexp.
  - `sigstore-policy-strata.yaml` — Sigstore Policy CR (alternative to Kyverno) doing the same.
- [ ] `docs/site/content/operate/_index.md` card grid extended with new entry pointing at `/operate/image-verification.md`.
- [ ] `make docs-build` green (no broken refs).
- [ ] Typecheck passes
- [ ] Tests pass

### US-011: Composite smoke + ROADMAP flip Done + PRD removal
**Description:** As an operator, I want end-to-end smoke validation that all features compose cleanly, plus the cycle ROADMAP entry flipped Done.

**Acceptance Criteria:**
- [ ] New script `scripts/smoke-supply-chain.sh`: (a) `make govulncheck` exits 0 (skip with WARN when govulncheck missing), (b) `make trivy-check` exits 0 (skip with WARN when trivy missing), (c) `make gosec` exits 0 (skip with WARN when gosec missing), (d) `make license-audit` exits 0 (skip with WARN when go-licenses missing), (e) `scripts/smoke-image-verification.sh` from US-008 exits 0, (f) `.github/dependabot.yml` parses as valid YAML (via `yq` — install fallback `apt-get install -y yq`).
- [ ] New `make smoke-supply-chain` Makefile target wraps the smoke.
- [ ] Verify ALL existing smokes still pass: `make smoke`, `make smoke-tikv-default-lab`, `make smoke-harden-gateway` (Cycle A), `make smoke-observability` (Cycle B), every `scripts/smoke-*.sh` shipped.
- [ ] **ROADMAP entry FLIPPED to Done** in `ROADMAP.md`: `~~**P0/P1 — Cycle C: supply-chain-security (govulncheck + trivy + gosec CI gates + Dependabot + SECURITY.md + first release tag + SLSA L3 provenance + SPDX SBOM + cosign signing + license audit).**~~ — **Done.** Shipped via ralph/supply-chain-security cycle (US-001..US-011). 3 parallel CI scanners gated (govulncheck HIGH+CRITICAL / trivy CRITICAL / gosec MEDIUM); license audit gates banned licenses (forbidden + restricted classes). Dependabot weekly for Go × 2 + Actions + npm + Docker (patch-only auto-merge, grouped per ecosystem). SECURITY.md with 90-day SLA + GHSA-only disclosure. First release tag v0.0.1-alpha.1 published; ghcr.io/danchupin/strata image signed via cosign keyless OIDC + SLSA L3 provenance + SPDX SBOM via slsa-framework/slsa-github-generator reusable workflow + anchore/sbom-action. Operator verify recipe at /operate/image-verification.md with Kyverno + Sigstore Policy admission control examples. Pre-launch hard cutover — no behavior change. (commit <SHA>)`
- [ ] All existing CI jobs still green; new `govulncheck` / `trivy-image` / `gosec` / `license-audit` jobs green on this PR.
- [ ] New `release-image.yml` workflow successfully executed for `v0.0.1-alpha.1` tag (verifiable via Actions tab + image visible at `ghcr.io/danchupin/strata:v0.0.1-alpha.1`).
- [ ] Delete `tasks/prd-supply-chain-security.md` in closing commit per PRD lifecycle.
- [ ] Typecheck + `make test-race` + `make docs-build` pass.

## Functional Requirements

### CVE / static-analysis scanners (US-001 + US-002 + US-003)
- FR-1: `govulncheck` runs on every PR as parallel job, scans main + cephimpl modules, hard-fails on HIGH/CRITICAL in `internal/`/`cmd/` callgraph (jq-based parser), WARNs on transitive/test-only.
- FR-2: `trivy image` runs on every PR as parallel job against built `strata:ceph`, hard-fails on CRITICAL, WARNs on HIGH-only, uploads SARIF to GitHub Code Scanning.
- FR-3: `gosec` runs on every PR as parallel job with `-severity=medium`, configurable excludes via `.gosec.yml`, uploads SARIF.
- FR-4: All scanners have local Makefile equivalents (`make govulncheck`, `make trivy-check`, `make gosec`) degrading to WARN + exit 0 when binary missing.
- FR-5: Severity classification helpers in shared `scripts/lib/severity.sh` — jq-based, reusable across all scanners.
- FR-6: All third-party Actions SHA-pinned (fetched at impl time, documented in commit message).
- FR-7: New jobs run in parallel (`needs: [web-build]` or `[docker-build]` only — NOT serial extension of `lint-build`).

### Dependabot (US-004)
- FR-8: `.github/dependabot.yml` covers 5 ecosystems (Go main, Go cephimpl, Actions, npm, Docker × 2 dirs).
- FR-9: Weekly schedule (Friday 06:00 UTC) per ecosystem.
- FR-10: Per-ecosystem grouping (patch group + minor-major group OR single group for low-churn ecosystems).
- FR-11: Patch-only updates auto-merge via `.github/workflows/dependabot-auto-merge.yml` IF CI green.
- FR-12: `open-pull-requests-limit: 5` per ecosystem.
- FR-13: Commit message convention `chore(deps): ...`.

### SECURITY.md (US-005)
- FR-14: Supported versions table + GHSA-only disclosure (no email backup).
- FR-15: 90-day SLA + 5-business-day ack + embargo policy.
- FR-16: In-scope + out-of-scope clarifications + Hall of Fame section.
- FR-17: README extended with `## Security` section linking SECURITY.md.

### First release + SLSA + SBOM + cosign + license audit (US-006..US-009)
- FR-18: `.github/workflows/release-image.yml` triggers on `push: tags: ['v*']` + `workflow_dispatch`.
- FR-19: SLSA L3 via `slsa-framework/slsa-github-generator/.github/workflows/generator_container_slsa3.yml` reusable workflow — orchestrates build + push + attest end-to-end (NOT split into separate steps).
- FR-20: First tag `v0.0.1-alpha.1` cut as part of US-006; image published to `ghcr.io/danchupin/strata:v0.0.1-alpha.1` + `:latest`.
- FR-21: SPDX 2.3 JSON SBOM via `anchore/sbom-action` consuming SLSA-pushed digest, uploaded as workflow artifact + Release asset.
- FR-22: cosign keyless OIDC signing of pushed image via `sigstore/cosign-installer`, consuming SLSA-pushed digest.
- FR-23: License audit via `go-licenses check ./...` (forbidden + restricted classes blocked), license report emitted as workflow artifact.
- FR-24: All operator verify recipes use anchored + escaped + suffix-matched regexps (`^https://github\.com/danchupin/strata/.*`).
- FR-25: `/operate/image-verification.md` documents full verify chain + k8s Kyverno + Sigstore Policy admission control examples.

### Smoke (US-011)
- FR-26: `scripts/smoke-supply-chain.sh` exercises all 4 feature areas locally (degrading to WARN when tooling missing per established pattern).
- FR-27: `make smoke-supply-chain` Makefile target wraps the smoke.
- FR-28: ROADMAP entry created in US-001 cycle-prep + flipped Done in US-011 closing commit with SHA backfill.
- FR-29: `release-image.yml` workflow successfully executed for `v0.0.1-alpha.1` tag — concrete evidence the pipeline works end-to-end.

## Non-Goals (Out of Scope)

- **Distroless image migration.** Cycle A2.
- **Docker Hub / quay.io mirroring.** ghcr.io-only this cycle.
- **CycloneDX SBOM format.** SPDX-only this cycle.
- **Static cosign key signing.** Keyless OIDC only.
- **Renovate.** Dependabot is built-in.
- **Sigstore policy bundle distribution.** Operators write their own admission control policies; we provide examples.
- **Scheduled trivy scan against published image.** Would catch CVEs that emerge AFTER release. Defer to follow-up cycle if needed.
- **License exception process automation.** `.licensei.yml` config supports manual per-package allowlist; cycle does not automate the review process.
- **Email backup disclosure channel.** GHSA-only.
- **WORM audit-log.** Cycle J.
- **PDB / NetworkPolicy / HPA.** Cycle D.
- **Chaos / fuzz tests.** Cycle F.

## Design Considerations

- **All gates are CI / release-engineering side.** Zero code-side behavior change.
- **Scanners as parallel jobs, not lint-build extensions.** Avoids inflating lint-build runtime from ~3 min to ~7-8 min; total wall-clock prirost ~30-60s.
- **SHA-pin all third-party actions.** Per supply-chain best practice — semver tag can be moved by maintainer to point at a compromised commit; SHA pin can't.
- **Severity gates pragmatic, not maximal.** govulncheck WARNs on transitive-only (waiting on upstream shouldn't block PRs); trivy WARNs on HIGH (CRITICAL only hard-fails — HIGH often includes unused base-image CVEs); gosec at MEDIUM (LOW too noisy without errcheck).
- **Keyless OIDC over static key.** No key-rotation runbook, no leak risk, GitHub Actions identity = audit trail via Fulcio + Rekor.
- **GHSA over email + GPG.** Single encrypted + audit-trailed channel; researchers don't juggle GPG keys.
- **SLSA L3 generator owns the build pipeline.** Splitting build/push/attest into separate steps breaks L3 guarantee (provenance must be service-generated within the verified build).
- **First-tag-and-publish ships within US-006.** Without an actual published image, US-008 verify recipe can't be smoke-tested against real cosign signatures.
- **License audit fail-on-forbidden, allow-by-exception.** `.licensei.yml` per-package allowlist with rationale supports legitimate edge cases (e.g. test-only dep with restricted license).
- **cosign verify regexp anchored + escaped + suffix-matched.** `^...$` + escaped dots + `/.*` suffix prevents Fulcio identity bypass via prefix collision (e.g. `https://github.com/danchupin/strata-evil`).
- **Smoke degrades gracefully when scanner binaries missing.** Mirrors `make helm-lint` / `make promtool-check` pattern.

## Technical Considerations

- **`govulncheck` install** via `go install`; ~5MB; ~30s scan on main + ~30s on cephimpl (separate `cd` per CLAUDE.md workspace MVS pitfall).
- **`aquasecurity/trivy-action`** pinned at SHA. Image scan ~1-2 min.
- **`securego/gosec/v2`** pinned at SHA. Source scan ~30s.
- **`slsa-framework/slsa-github-generator`** reusable workflow at `.github/workflows/generator_container_slsa3.yml@<sha>`. Adds ~2 min to release workflow.
- **`anchore/sbom-action`** syft-based SBOM. Adds ~30s.
- **`sigstore/cosign-installer`** keyless signing. Signing step adds ~10s.
- **`go-licenses`** install via `go install`; license check ~10s on main module.
- **`jq`** for JSON parsing — available on ubuntu-latest runner; install fallback documented.
- **GitHub `id-token: write` permission** required for OIDC keyless signing — set per-workflow (`release-image.yml`), NOT repository-wide.
- **GHCR auth** — workflow uses `GITHUB_TOKEN` with `packages: write` permission. No separate PAT needed.
- **`security-events: write`** permission required for SARIF upload (govulncheck, trivy, gosec jobs).
- **Trivy + gosec SARIF** uploaded via `github/codeql-action/upload-sarif@<sha>` — pinned.
- **Dependabot config schema** validated by GitHub on PR commit.
- **CI concurrency budget** — Cycle C adds 4 new parallel jobs (govulncheck, trivy-image, gosec, license-audit). Total parallel jobs in `ci.yml` ~12. Free-tier GitHub Actions limit = 20 concurrent. Headroom OK.

## Success Metrics

- All 11 user stories complete (`passes=true` in `scripts/ralph/prd.json`).
- ROADMAP `Cycle C: supply-chain-security` entry created in US-001 + flipped Done in US-011 with SHA backfill.
- `make smoke-supply-chain` green locally.
- All existing smokes + CI jobs still green (no regression).
- New parallel CI jobs (`govulncheck`, `trivy-image`, `gosec`, `license-audit`) green on the cycle PR.
- `SECURITY.md` published at repo root; GitHub repo Security tab shows the disclosure policy.
- `ghcr.io/danchupin/strata:v0.0.1-alpha.1` exists, signed by GitHub Actions identity (verifiable via `cosign verify`), SLSA L3 provenance attached, SPDX SBOM attached to GitHub Release.
- A security researcher following `SECURITY.md` can file a report via GHSA from zero in ≤2 minutes.
- An SRE following `/operate/image-verification.md` can verify image signature + SBOM + license in ≤5 minutes.

## Open Questions

- **`anchore/sbom-action` vs `docker buildx imagetools` for SBOM** — both emit SPDX; syft (Anchore) has broader format support + better Go module accuracy. Stick with syft unless Ralph discovers a blocker.
- **Rekor transparency log query depth** — `cosign verify` checks Rekor by default; document deeper forensic recipes in `/operate/image-verification.md`.
- **Policy bundle versioning** — admission controller manifests in `deploy/k8s/admission-controllers/` are examples, not authoritative. Document expected drift.
- **Tagging cadence** — Cycle C cuts `v0.0.1-alpha.1`. Future cycle cadence (per cycle? per quarter? on demand?) deferred until pre-launch policy matures.
- **MPL-2.0 license treatment** — flagged for case-by-case review in US-009; document the per-dep verdict in `.licensei.yml` at impl time.
- **License audit on cephimpl module** — separate `cd internal/data/rados/cephimpl && go-licenses check ./...` run; document any cephimpl-specific exceptions in commit message.
