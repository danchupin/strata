---
title: 'Image verification'
weight: 55
description: 'Verify the cosign signature, SLSA provenance, SBOM, and license inventory of a published Strata image before deploying — plus Kubernetes admission-control examples (Kyverno + Sigstore policy-controller) that gate deploys on signature validity, a Rekor forensic-query recipe, and a troubleshooting guide for the common verify failures.'
---

# Image verification

Every Strata image published to `ghcr.io/danchupin/strata` is signed,
attested, and inventoried by the release pipeline
(`.github/workflows/release-image.yml`). This page is the operator
playbook for verifying that supply-chain evidence **before** the image
reaches a node, plus two Kubernetes admission-control shapes that make
the check mandatory at deploy time.

The pipeline produces four pieces of evidence, all anchored to the same
image digest:

| Evidence | Producer | Story | Verified with |
|----------|----------|-------|---------------|
| cosign signature (keyless OIDC) | `sigstore/cosign` | US-008 | `cosign verify` |
| SLSA L3 provenance | `slsa-github-generator` | US-006 | `cosign verify-attestation` |
| SPDX 2.3 SBOM | `anchore/sbom-action` (syft) | US-007 | `grype` / `trivy sbom` |
| License inventory | `go-licenses` | US-009 | `go-licenses report` |

## What signing buys you

Keyless signing gives you a **non-repudiable provenance trail from source
to running image**: the GitHub Actions OIDC identity that built the image
is the signer, Fulcio issues a short-lived certificate bound to that
identity, and the signature lands in the Rekor public transparency log.
Because the signature covers the image **digest** (not a mutable tag), a
verified image is tamper-proof — repush a different layer and the
signature no longer matches. There is **no maintainer-held private key to
leak or rotate**; the trust root is GitHub's OIDC issuer plus the public
Sigstore infrastructure. The Rekor entry is append-only and publicly
queryable, so a forensic investigation can prove exactly which workflow
run, at which commit, produced a given digest — even years later.

## Verify recipe

Run the full chain before deploying. Replace `v0.0.1-alpha.1` with the
tag you are about to ship; the first released tag is `v0.0.1-alpha.1`.

```bash
TAG=v0.0.1-alpha.1
IMAGE=ghcr.io/danchupin/strata:${TAG}
```

### (a) cosign signature — keyless OIDC identity

The regexp pair is **anchored** (`^…$`), **escaped** (`\.`), and
**suffix-matched** (`/.*`) so it accepts any workflow path under the
repo while rejecting look-alike identities (`danchupin/strata-evil`,
`evil-danchupin/strata`). Copy-paste-runnable:

```bash
cosign verify \
  --certificate-identity-regexp='^https://github\.com/danchupin/strata/.*' \
  --certificate-oidc-issuer-regexp='^https://token\.actions\.githubusercontent\.com$' \
  "${IMAGE}"
```

A pass prints the verified signature(s) and the certificate subject. Any
identity or issuer mismatch exits non-zero — never deploy on a non-zero
exit.

### (b) SLSA provenance attestation

Proves the image was built by the SLSA L3 generator, not a local
`docker push`:

```bash
cosign verify-attestation \
  --type slsaprovenance \
  --certificate-identity-regexp='^https://github\.com/danchupin/strata/.*' \
  --certificate-oidc-issuer-regexp='^https://token\.actions\.githubusercontent\.com$' \
  "${IMAGE}"
```

### (c) SBOM vulnerability scan

Download the SPDX SBOM attached to the GitHub Release and scan it for
CVEs:

```bash
gh release download "${TAG}" -p '*sbom*' -R danchupin/strata
grype "sbom:strata-${TAG}-sbom.spdx.json"
# or:  trivy sbom "strata-${TAG}-sbom.spdx.json"
```

### (d) License inventory check

Verify the SBOM carries no banned (forbidden / restricted) licenses. The
release CSV inventory is the authoritative artifact (US-009); to
re-derive from source at a tag:

```bash
git checkout "${TAG}"
go-licenses report ./... 2>/dev/null > license-report.csv
go-licenses check ./... --disallowed_types=forbidden,restricted
```

A zero exit confirms no GPL / AGPL / LGPL / CDDL / EPL imports slipped in.

## Rekor transparency-log query

For forensic use, query Rekor directly by the image digest — this proves
a signature entry exists in the public append-only log independent of the
registry:

```bash
DIGEST=$(cosign triangulate --type digest "${IMAGE}")   # sha256:…
rekor-cli search --sha "${DIGEST#sha256:}"
# inspect a returned log index:
rekor-cli get --log-index <index>
```

The returned entry carries the Fulcio certificate (signer identity), the
inclusion proof, and the signed-entry timestamp — sufficient to attribute
the build to a specific workflow run.

## Kubernetes admission control

Make verification mandatory: reject any pod whose image is not a validly
signed `ghcr.io/danchupin/strata` digest. Two equivalent shapes ship in
[`deploy/k8s/admission-controllers/`](https://github.com/danchupin/strata/tree/main/deploy/k8s/admission-controllers)
— pick whichever policy engine your cluster already runs.

### Kyverno

[`kyverno-strata-image-signed.yaml`](https://github.com/danchupin/strata/blob/main/deploy/k8s/admission-controllers/kyverno-strata-image-signed.yaml)
is a `ClusterPolicy` with a `verifyImages` rule. It matches the
`ghcr.io/danchupin/strata*` glob, requires a keyless cosign signature
whose certificate identity + issuer match the same anchored regexps as
the CLI recipe, and mutates the image reference to its verified digest so
later layer swaps cannot bypass the check:

```yaml
verifyImages:
  - imageReferences:
      - "ghcr.io/danchupin/strata*"
    attestors:
      - entries:
          - keyless:
              subject: "https://github.com/danchupin/strata/*"
              issuer: "https://token.actions.githubusercontent.com"
```

Install Kyverno, then `kubectl apply -f
deploy/k8s/admission-controllers/kyverno-strata-image-signed.yaml`. With
`failurePolicy: Fail` an unsigned image is rejected at admission.

### Sigstore policy-controller

[`sigstore-policy-strata.yaml`](https://github.com/danchupin/strata/blob/main/deploy/k8s/admission-controllers/sigstore-policy-strata.yaml)
is a `ClusterImagePolicy` (the Sigstore policy-controller CRD). Same
contract, native Sigstore shape:

```yaml
authorities:
  - keyless:
      identities:
        - issuer: "https://token.actions.githubusercontent.com"
          subjectRegExp: "^https://github\\.com/danchupin/strata/.*"
```

Install the policy-controller, label the namespace
(`policy.sigstore.dev/include: "true"`), then apply the CR. Pods pulling
an unsigned Strata image are denied.

## Troubleshooting

| Error | Cause | Fix |
|-------|-------|-----|
| `certificate verification failed` / identity mismatch | The `--certificate-identity-regexp` or `--certificate-oidc-issuer-regexp` does not match the signer. | Copy the regexp pair from the [Verify recipe](#verify-recipe) verbatim — anchored, escaped, suffix-matched. A look-alike repo (`strata-foo`) is correctly rejected. |
| `no matching attestations` | You ran `verify-attestation` against an image that has a signature but no SLSA provenance, or a wrong `--type`. | Use `--type slsaprovenance`. Older locally-built images have no provenance — only pipeline-released tags do. |
| `image not signed` / `no signatures found` | The tag predates the signing pipeline, was built locally, or you verified a mutable tag that was repushed unsigned. | Verify a pipeline-released tag (`v0.0.1-alpha.1` onward). Pin to a digest, not a floating tag. |
| `Rekor entry not found` | Sigstore's transparency log was unreachable at verify time, or you searched the wrong digest. | Re-run with network access to `rekor.sigstore.dev`; confirm the digest via `cosign triangulate`. For air-gapped verify, attach a bundled Rekor proof with `--bundle`. |
| `error getting fulcio roots` / TUF init hang | First-run TUF metadata fetch is blocked or slow. | Ensure egress to `tuf-repo-cdn.sigstore.dev`; pre-warm with `cosign initialize`. |

## See also

- [TLS termination + backend mTLS]({{< relref "/operate/tls-termination" >}})
  — put the gateway on the wire securely.
- [Deploy]({{< relref "/deploy/" >}}) — Kubernetes and compose
  deployment shapes.
- The release pipeline that produces this evidence:
  [`.github/workflows/release-image.yml`](https://github.com/danchupin/strata/blob/main/.github/workflows/release-image.yml).
