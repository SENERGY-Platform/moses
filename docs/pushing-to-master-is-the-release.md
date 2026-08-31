# Pushing to master is the release

**Applies when** releasing this service. **Delimitation:** how the built image
reaches a cluster is deployment configuration and lives outside this
repository; this document ends at the registry.

## The workflow assigns the version — never tag manually

`.github/workflows/prod.yml` runs on every push to master. Its
`mathieudutour/github-tag-action` step computes the next version from the
commit messages (conventional commits: `feat:` bumps minor, `fix:` bumps
patch), **creates the git tag itself**, and the image is built and pushed under
exactly that tag, plus `prod` and `latest`.

A manually pushed tag races this. Observed 2026-08-26: a hand-made `v0.11.0`
plus the master push produced three tags on one commit (`v0.11.0`–`v0.11.2`),
and only the action's tags had images — a deployment pinned to `v0.11.0`
failed with:

```
Failed to pull image "ghcr.io/senergy-platform/moses:v0.11.0": ... not found
```

So: push master, then read the version the action created (`git ls-remote
--tags origin` or the workflow run) and pin deployments to that.

## Consequences

- The version to deploy exists only *after* the workflow ran — wait for the
  `PROD Docker Image` run to finish before pinning.
- Every push to master publishes an image. There is no release branch and no
  manual release step.
