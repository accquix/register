# Publication and contribution security checklist (OQ-24)

The repository became public on 2026-11-11, and the steps below were applied that day. On GitHub Free for organisations, branch protection, rulesets,
fork-PR approval and private vulnerability reporting are available for **public** repositories
only (checked 2026-11-11: the API answers 403 "Upgrade to GitHub Pro or make this repository
public", or 422 for fork-PR approval). So these steps happen at publication, in this order.

## Contribution model (decided)

- **Issues: open.** Bug reports and installation problems.
- **Pull requests: open, without a promise to merge.** Accquix keeps full discretion over the
  roadmap and over what is merged (CONTRIBUTING.md, the pull request template).
- **Discussions: off.** No extra channel to look after.
- **Security: private vulnerability reporting** (SECURITY.md), never public issues.

## Already in place (private repository)

- [x] Actions default token is read-only and cannot approve pull requests
      (`default_workflow_permissions: read`, `can_approve_pull_request_reviews: false`).
- [x] Every workflow sets `permissions:` explicitly and uses `pull_request`, never
      `pull_request_target`; fork runs get no secrets and a read-only token.
- [x] CONTRIBUTING.md policy, pull request template, issue forms, SECURITY.md, code of conduct.

## Before making the repository public

- [x] OQ-2, OQ-16 and OQ-21 done, or consciously deferred (D30).
- [x] Scan the whole git history for secrets and real register data (for example `gitleaks
      detect` over all commits). The history becomes public as it is.
- [x] The documentation site deploys (Cloudflare builds it from the repository; register.accquix.com
      points to it).

## Right after making it public

1. **Private vulnerability reporting**

   ```sh
   gh api -X PUT repos/accquix/register/private-vulnerability-reporting
   ```

   Then check that the issue form's "Report a security vulnerability" link opens the form.

2. **Protect `main`.** Pull requests required, CI required, **0 approving reviews** (a single
   maintainer cannot approve their own pull request), no force pushes, no deletion, conversations
   resolved, admins may bypass in an emergency (`enforce_admins: false`).

   ```sh
   gh api -X PUT repos/accquix/register/branches/main/protection --input - <<'JSON'
   {
     "required_status_checks": { "strict": false, "contexts": ["test (16)", "test (17)", "test (18)", "lint", "vulncheck", "compose-smoke"] },
     "enforce_admins": false,
     "required_pull_request_reviews": { "required_approving_review_count": 0 },
     "restrictions": null,
     "allow_force_pushes": false,
     "allow_deletions": false,
     "required_conversation_resolution": true
   }
   JSON
   ```

   The required checks are the jobs of `ci.yml`, by their full names: a matrix job is one check
   per entry (`test (16)`, `test (17)`, `test (18)`). The `docs` workflow runs only when the site
   changes, so it is not required.

3. **Protect release tags.** Only repository admins may create, move or delete `v*` tags, because a
   tag publishes binaries and the image.

   ```sh
   gh api -X POST repos/accquix/register/rulesets --input - <<'JSON'
   {
     "name": "release tags",
     "target": "tag",
     "enforcement": "active",
     "conditions": { "ref_name": { "include": ["refs/tags/v*"], "exclude": [] } },
     "rules": [{ "type": "creation" }, { "type": "update" }, { "type": "deletion" }, { "type": "non_fast_forward" }],
     "bypass_actors": [{ "actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always" }]
   }
   JSON
   ```

   (`actor_id` 5 is the built-in Admin repository role.)

4. **Approve CI for every external contributor.** Workflows from forks run only after a
   maintainer approves them, also for people whose earlier pull request was merged.

   ```sh
   gh api -X PUT repos/accquix/register/actions/permissions/fork-pr-contributor-approval \
     -f approval_policy=all_external_contributors
   ```

   Read the diff before approving a run: a read-only token without secrets limits the damage,
   but still runs someone else's code.

5. **Check the settings once more:** Issues on, Discussions off, Wiki off, "Allow auto-merge" off.

## Before the first public release

- [x] The tag ruleset above is active; create the first `v*` tag as an admin.
- [x] The release workflow ran once on a test tag in a fork, or its first run is watched. (v1.0.0-rc.1 on
  2026-11-12, watched: binaries, checksums, deploy archive and the multi-arch image with SBOM and
  provenance verified.)
- [x] GHCR package visibility set to public after the first image push. (2026-11-12; the organisation
  first had to allow public packages under Settings → Packages.)
