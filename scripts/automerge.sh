#!/usr/bin/env bash
# Merge a pull request a workflow has just opened on the index, once main's required
# checks pass, and publish the result. index-discover.yml (new entries) and
# index-check.yml (dead ones) both end with this.
#
#   scripts/automerge.sh <pr-number> <branch>
#
# Needs GH_TOKEN with contents, pull-requests and actions write, and GH_REPO.
#
# Merged here, synchronously, rather than with GitHub's auto-merge: a merge done with
# GITHUB_TOKEN starts no push workflows, so pages.yml would never publish the index.
# This merges, then starts the deploy by hand -- workflow_dispatch is the one event that
# token may start.
set -euo pipefail

[ $# -eq 2 ] || { echo "usage: scripts/automerge.sh <pr-number> <branch>" >&2; exit 2; }
pr="$1" branch="$2"
: "${GH_REPO:?GH_REPO must name the repository}"
required=4   # main's branch protection: test x2, goreleaser-check, toyfile

# GitHub creates the pull request's own checks but holds them for a maintainer's
# approval, because github-actions[bot] opened it. Approve whatever is held -- asked
# again on every pass, since the runs do not all appear at once.
approve_held() {
  local id
  for id in $(gh run list --branch "$branch" --json databaseId,conclusion \
                --jq '.[] | select(.conclusion == "action_required") | .databaseId'); do
    gh api -X POST "repos/$GH_REPO/actions/runs/$id/approve" >/dev/null \
      || echo "could not approve run $id; approve it on the pull request"
  done
}

# The required checks exist once the approved runs start; wait for them to exist, then
# for them to finish.
for _ in $(seq 1 60); do
  approve_held
  have=$(gh pr checks "$pr" --required --json name --jq 'length' 2>/dev/null || echo 0)
  [ "$have" -ge "$required" ] && break
  sleep 15
done
if ! gh pr checks "$pr" --required --watch --interval 30; then
  gh pr comment "$pr" --body "Not merged automatically: a required check failed."
  exit 1
fi
gh pr merge "$pr" --rebase
gh workflow run pages.yml --ref main
echo "merged #$pr and started the site deploy"
