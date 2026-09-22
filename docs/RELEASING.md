# Preparing and publishing releases

dworm versions come from Git tags. There is no source-code version constant to
bump: local Makefile builds derive their version from `git describe`, and the
GitHub Actions release workflow injects the pushed tag into the host binary.

## Prepare

1. Choose a version. Use a minor release for new functionality; project environment
   configuration shipped in v0.5.0; self-update and embedded endpoints use **v0.6.0**.
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

For the prepared v0.6.0 commit:

```bash
git tag -a v0.6.0 -m "v0.6.0"
git push origin HEAD
git push origin v0.6.0
```

Use the repository's configured signing settings when creating tags.
The [release workflow](../.github/workflows/release.yml) builds Linux and macOS
host binaries for amd64 and arm64. Before each host build, `make prepare-endpoints`
cross-compiles stripped, static Linux amd64 and arm64 endpoints. Both are embedded
in every host; archives contain only `dworm` and documentation/license files.
The archive layout remains `dworm_<tag>_<os>_<arch>/dworm`, with SHA-256 entries
for each archive in `checksums.txt`.
The workflow uploads platform archives and checksums, and creates a GitHub release
using the version's prepared notes plus generated commit notes. Tags without a
matching notes file retain generated release notes.

After the workflow succeeds, inspect the release assets and verify that the
downloaded host reports the expected version with `dworm --version`.

The first embedded-endpoint release must be installed using the installer or
manually. Subsequent stable releases can be installed with `dworm self-update`.
It uses go-selfupdate v1.5.2 with a mandatory `checksums.txt` validator for both
discovery and installation. Source review of that version confirms replacement
uses mode 0755 and same-directory rename, with rollback attempted on final rename
failure. dworm resolves symlinks before calling it and reports rollback failures.
Linux fixture tests exercise actual replacement; macOS cross-builds alone are
not macOS runtime replacement validation.

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

## v0.6.0 validation record

- Go unit/race tests and `go vet ./...` passed. HTTP fixtures cover all four host
  asset names, nested extraction, stable selection, current/newer versions,
  checksum absence/mismatch, network/platform errors, directory permissions,
  symlink preservation, and execution of the installed fixture.
- Embedded payload tests inspect ELF machine types for both architectures;
  injection tests verify image-ID inspection and temporary cleanup on success,
  copy failure, and publish failure.
- Linux amd64 native builds and Linux arm64/macOS amd64/macOS arm64 cross-builds
  passed. No macOS runtime replacement or ARM64 container execution was tested;
  this runner has no ARM64 emulation registered.
- `python3 test/install_test.py` passed for single-binary and legacy two-binary
  archives, including preserving old endpoints for new installations.
- The isolated `test-embedded-endpoint.sh` passed with only a copied host binary:
  synthetic git credentials and port forwarding work after atomic replacement of
  the executing endpoint file. This tests replacement continuity with the same
  payload, not an end-to-end upgrade between two published endpoint revisions.
- The broader Docker suite passed port forwarding, environment variables,
  project environment refresh, and rebuild. SSH failed its existing plain
  `docker exec` environment assertion; GPG failed installing missing tools;
  removal hit an image referenced by multiple tags; real git credentials skipped
  because no host helper was configured. The full suite is not claimed to pass.
