---
title: 'Bench runner setup (self-hosted)'
weight: 50
description: 'Provision a self-hosted GitHub Actions runner that drives the weekly rgw-comparison bench cycle.'
---

# Bench runner setup (self-hosted)

The weekly RGW comparison bench
([`.github/workflows/bench-rgw.yml`]({{< relref "/architecture/benchmarks/rgw-comparison" >}}))
runs on a self-hosted GitHub Actions runner labelled
`[self-hosted, strata-bench]`. The runner is operator-provisioned once;
afterwards the cron + manual-dispatch triggers fire on schedule with zero
human input. Auto-PRs land against `main` carrying refreshed numbers and
the runner's specs.

This page covers the one-time provision. Closes ROADMAP P3
_"bench-rgw lima envelope fix"_ via US-005 of cycle
`ralph/auth-dx-trailer-lima`.

## Required hardware

The bench cycle exercises an 11-workload sweep with multipart uploads up
to 25 GiB transient and a 100 k-key list seed. The lima dev box does not
have the RAM envelope for the full sweep
([`rgw-comparison.md` Limitations](./../architecture/benchmarks/rgw-comparison/#limitations)).
A dedicated Linux box clears the blocker.

| Component | Minimum | Notes                                                   |
| --------- | ------- | ------------------------------------------------------- |
| CPU       | 8 cores | Strata replicas + RGW + warp + ceph OSD share the box.  |
| RAM       | 16 GiB  | Lima default of 9.7 GiB cannot host the full sweep.     |
| Disk      | 200 GiB | Multipart-5g × 2 targets × 3 runs ≈ 150 GiB transient.  |
| OS        | Ubuntu 22.04 LTS (or equivalent) | Other Linux distros OK if Docker + GitHub Actions runner work. |
| Docker    | 24.x or newer | Compose v2 required.                              |
| Network   | Outbound to `github.com`, `quay.io`, `hub.docker.com`. | Inbound not needed. |

A bare-metal box or a long-lived cloud VM both work. The runner does not
need a public IP — GitHub Actions polls outward.

## Step 1 — Install Docker + tooling

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl gnupg make jq awscli
# Docker engine — follow upstream docker-ce install for your distro.
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"
# log out / log back in so the docker group membership takes effect
```

Verify:

```bash
docker version            # 24.x or newer
docker compose version    # v2.x
make --version            # GNU make 4.x
jq --version
aws --version             # awscli v2 recommended
```

## Step 2 — Register the self-hosted runner

Follow the upstream
[GitHub Actions self-hosted runner](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/adding-self-hosted-runners)
guide. Critical bits for this workflow:

1. Generate a runner registration token from
   `Repo Settings → Actions → Runners → New self-hosted runner`. Token is
   single-use; rotate via the same UI if it expires before activation.
2. On the bench box:

   ```bash
   mkdir -p ~/actions-runner && cd ~/actions-runner
   curl -o actions-runner-linux-x64.tar.gz -L \
     https://github.com/actions/runner/releases/download/v2.319.1/actions-runner-linux-x64-2.319.1.tar.gz
   tar xzf actions-runner-linux-x64.tar.gz
   ./config.sh \
     --url https://github.com/<org>/strata \
     --token <token-from-step-1> \
     --name strata-bench-runner-1 \
     --labels self-hosted,strata-bench \
     --work _work
   ```

   Both labels must be present — the workflow targets `runs-on:
   [self-hosted, strata-bench]`.
3. Install as a systemd service so the runner survives reboots:

   ```bash
   sudo ./svc.sh install $(whoami)
   sudo ./svc.sh start
   sudo ./svc.sh status
   ```

## Step 3 — Verify with a manual dispatch

From a developer workstation with `gh` configured:

```bash
gh workflow run bench-rgw.yml --ref main
gh run watch                    # or: gh run list --workflow=bench-rgw.yml
```

Expected timeline:

- ~5 min for `make up-all` + `make wait-tikv` + `make wait-ceph` +
  `make wait-strata-lab`.
- ~5 min for `make up-bench-rgw` + RGW realm bootstrap.
- ~120 min for `make bench-rgw-comparison` (11 workloads × 2 targets ×
  3 runs).
- ~30 s for `scripts/bench-update-doc.sh` + PR open.

Total budget: 240 minutes per the workflow's `timeout-minutes: 240`. If
the run hangs past this ceiling investigate `~/actions-runner/_diag/` on
the bench box.

## Step 4 — Auto-PR behaviour

When the doc changes the workflow opens
`bench/rgw-comparison-<YYYY-MM-DD>` PR against `main`. The auto-merge
gate watches the Strata-side max p99 regression vs the previously
committed baseline
(`docs/site/content/architecture/benchmarks/data/rgw-bench-baseline.json`):

- **Regression < `BENCH_REGRESSION_THRESHOLD`** (default 10 %, tunable
  via the manual-dispatch input or the workflow's env): `gh pr merge
  --auto --squash --delete-branch` arms. Once the
  `branch-protection-required-checks` pass the PR squash-merges on its
  own.
- **Regression ≥ threshold**: PR stays open with a
  `@danchupin` comment. Operator reviews the diff + decides to merge or
  bisect.

The first run on a clean repo has no baseline → regression is reported
as `0.00 %` and the PR always auto-merges.

## Step 5 — Lima box reduced smoke

The lima dev box still runs a reduced smoke (`make smoke-rgw-lab-restart`
+ 1-workload bench) for fast operator iteration:

```bash
make up-all && make wait-tikv && make wait-ceph && make wait-strata-lab
make up-bench-rgw && make wait-rgw
bash scripts/bench-rgw-comparison.sh --extract-rgw-creds
PUT_SMALL_DURATION=10 \
  bash scripts/bench-rgw-comparison.sh put-small both --runs=1 --concurrency=8
bash scripts/bench-rgw-comparison.sh --report
```

Use this for chasing a code-side regression locally before the next
cron fires. The doc-update path (`scripts/bench-update-doc.sh`) works
against the lima JSONL too — runs `--check` to verify drift without
modifying the doc.

## Troubleshooting

- **Runner stuck in "queued"**: labels mismatched. Re-register with
  `--labels self-hosted,strata-bench` (both required).
- **`make bench-rgw-comparison` aborts on `STRATA_BENCH_MIN_DISK_GB`**:
  pre-flight refuses to run below 300 GiB free. Either free disk or
  pass `STRATA_BENCH_MIN_DISK_GB=200` (workflow inherits env via repo
  secrets if you must).
- **PR didn't open after a green bench**: workflow short-circuits when
  `scripts/bench-update-doc.sh` reports `changed=false` — the new
  numbers were byte-identical to the previous baseline. Check the
  workflow's "Regenerate doc block + baseline" step output.
- **`gh pr create` returns "GraphQL: Resource not accessible by
  integration"**: the workflow's `permissions:` block grants
  `pull-requests: write`. If the org disables `GITHUB_TOKEN` PR
  creation, swap to a PAT via repo secret `BENCH_PR_TOKEN` and replace
  `${{ secrets.GITHUB_TOKEN }}` in the workflow.

## See also

- [RGW comparison]({{< relref "/architecture/benchmarks/rgw-comparison" >}})
  — the doc the workflow refreshes.
- [`scripts/bench-rgw-comparison.sh`](https://github.com/danchupin/strata/blob/main/scripts/bench-rgw-comparison.sh)
  — bench driver invoked by the workflow.
- [`scripts/bench-update-doc.sh`](https://github.com/danchupin/strata/blob/main/scripts/bench-update-doc.sh)
  — doc + baseline generator with `--check` mode for drift detection.
