# Summary

Preview tracks the bleeding edge of this repo so a regression surfaces there
before it can reach a stable channel. The image already had a `next` tag from
**Build Next**, but the npm SDK did not. Preview agents therefore built against
a stable client while their sidecar ran unreleased code, which is exactly the
mismatch a preview environment exists to catch.

Every push to `main` that touches `sdk/node` or `proto` now publishes the SDK
under the `next` dist-tag. The npm and PyPI publishers also fire on a merge that
touches their SDK, so bumping a version on `main` is the whole release act
rather than a dispatch someone has to remember.

# Design

**Both channels live in `publish-npm.yml`.** npm registers a trusted publisher
against `(repo, workflow filename, environment)`, so a workflow per channel
would need a second registration on npmjs.com and would publish nothing until
someone made it. One file, two jobs.

**The prerelease targets the next patch.** A stable `0.2.0` produces
`0.2.1-next.<sha>`. Basing it on the current version instead would sort `next`
*below* `latest`, so anyone comparing the two would read the bleeding edge as
the older build. Targeting the next patch also leaves room for the eventual
`0.2.1` release to sort above the prereleases it followed.

**Keyed on the commit, not a counter.** Re-running a commit resolves to the same
version, which the already-published check then skips. A counter would mint a
new version for work that had not changed.

**The bump stays on the runner.** `npm version --no-git-tag-version` writes
`package.json` in the workspace only, so `main` keeps the stable version and the
two channels never contend for the same field.

**No `next` channel on PyPI.** PyPI has no dist-tags. The analogue is a `.devN`
prerelease stream, which needs a convention for which version the dev releases
target and a guarantee that stable releases stay ahead of it. That is a separate
decision, so preview tracks the npm tag only.

# Migration

None. `Build Next` and `Build Latest` are unchanged, and the `latest` npm and
PyPI channels publish exactly the versions they did before.

Consumers see a new `next` dist-tag. Nothing resolves it unless it asks for it
by name: `npm install` without a tag still takes `latest`, and a caret range
never matches a prerelease.
