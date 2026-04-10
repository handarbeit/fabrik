# Releasing Fabrik

Fabrik releases are built in this private repo (`tenaciousvc/fabrik`) and published to the public distribution repo (`shadoworg/fabrik`). This document covers the release process and the `PUBLIC_REPO_RELEASE_TOKEN` PAT that makes cross-repo publishing work.

## How Releases Work

1. Commit `release-notes.md` to main with the release notes for the new version.
2. Tag the commit: `git tag v0.x.y && git push origin main v0.x.y`
3. The `release.yml` GitHub Actions workflow triggers on the tag push.
4. goreleaser builds binaries for `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`, packages them as `.tar.gz`, generates `checksums.txt`, and publishes a GitHub Release on **`shadoworg/fabrik`** (not this repo).
5. goreleaser also creates a matching tag on `shadoworg/fabrik` as part of the release.

Pre-release tags (those containing `-`, e.g. `v0.1.0-rc1`) are automatically marked as pre-releases by goreleaser (`prerelease: auto`).

## PUBLIC_REPO_RELEASE_TOKEN

goreleaser needs write access to `shadoworg/fabrik` to publish releases. This is provided via a fine-grained PAT stored as a repository secret.

### Required PAT Permissions

The PAT must be scoped to the **`shadoworg/fabrik`** repository with:
- **Contents**: Read and write (to create releases and upload artifacts)
- **Metadata**: Read-only (required by all fine-grained PATs)

### Creating the PAT

1. Go to GitHub Settings → Developer settings → Personal access tokens → Fine-grained tokens.
2. Click **Generate new token**.
3. Set **Resource owner** to `shadoworg`.
4. Set **Repository access** to **Only select repositories** → `shadoworg/fabrik`.
5. Under **Permissions → Repository permissions**, set **Contents** to **Read and write**.
6. Set an expiry of up to 1 year. **Record the expiry date** (see Rotation below).
7. Click **Generate token** and copy the value immediately.

### Storing the Secret

In `tenaciousvc/fabrik` → Settings → Secrets and variables → Actions:
- Name: `PUBLIC_REPO_RELEASE_TOKEN`
- Value: the PAT value copied above

### Rotating the PAT

Fine-grained PATs expire (maximum 1 year). When `PUBLIC_REPO_RELEASE_TOKEN` expires, goreleaser will fail with a 401 error and the release workflow will appear to succeed but no release will appear on `shadoworg/fabrik`.

**Rotation checklist:**
1. Create a new fine-grained PAT with the same permissions (see above).
2. Update the `PUBLIC_REPO_RELEASE_TOKEN` secret in `tenaciousvc/fabrik`.
3. Delete the old PAT from GitHub Settings.
4. Record the new expiry date in this section: **Current PAT expires: _____________**

Set a calendar reminder at least 2 weeks before expiry.

## Release History

GitHub Releases are published exclusively to `shadoworg/fabrik`. Internal team members should check the [shadoworg/fabrik Releases page](https://github.com/shadoworg/fabrik/releases) for release history and binary downloads.

No GitHub Releases are created on this (`tenaciousvc/fabrik`) repo.

## Migration Note

Users on versions of `fabrik` older than the version that introduced this cross-repo redirect will have `fabrikOwner = "tenaciousvc"` compiled in. Their auto-upgrade feature will check `tenaciousvc/fabrik` (which has no releases) and silently skip upgrading. Communicate the release location change to affected users so they can manually download the latest binary from `shadoworg/fabrik`.
