# Preparing and publishing releases

dworm versions come from Git tags. There is no source-code version constant to
bump: local Makefile builds derive their version from `git describe`, and the
GitHub Actions release workflow injects the pushed tag into the host binary.

## Prepare

1. Choose a version. Use a minor release for new functionality; project environment
   configuration and refreshable host commands are prepared as **v0.5.0**.
2. Update the README and add user-facing notes at `docs/releases/vX.Y.Z.md`.
   Include any host/endpoint compatibility or upgrade instructions.
3. Run the relevant checks:

   ```bash
   make test-race
   make build
   ./test/e2e/test-env-vars.sh
   ./test/e2e/test-project-env.sh
   git diff --check
   ```

   Run the broader Docker suite when validating changes to forwarding or container
   lifecycle behavior. Conditional credential tests depend on host credentials
   and the appropriate tools inside the container.
4. Review the diff and commit the implementation, tests, and documentation.

Release preparation ends with the reviewed commit. Publication is triggered by
pushing its version tag.

## Publish

For the prepared v0.5.0 commit:

```bash
git tag -a v0.5.0 -m "v0.5.0"
git push origin HEAD
git push origin v0.5.0
```

Use the repository's configured signing settings when creating tags.
The [release workflow](../.github/workflows/release.yml) builds Linux and macOS
host binaries for amd64 and arm64. Each archive includes the Linux amd64 endpoint.
The workflow uploads platform archives and checksums, and creates a GitHub release
using the version's prepared notes plus generated commit notes. Tags without a
matching notes file retain generated release notes.

After the workflow succeeds, inspect the release assets and verify that the
downloaded host reports the expected version with `dworm --version`.

## v0.5.0 validation record

- `make test-race` and `make build` passed during implementation.
- Release-preparation cross-builds passed for macOS amd64/arm64 and Linux arm64,
  completing the host platform matrix alongside the native Linux amd64 build.
- Independent Bash-session tests passed, including refreshed values, restored
  defaults, literal quoting, preserved prompt hooks, and pinned overrides.
- `test-env-vars.sh` and the isolated `test-project-env.sh` passed with Docker.
- The broader Docker suite was not fully successful: the SSH test queried plain
  `docker exec` instead of the environment-aware launcher; the GPG test failed
  while attempting to install missing container tools; the run timed out during
  rebuild. This is not a claim that the full E2E suite passed.
