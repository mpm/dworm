#!/bin/bash
# Skip condition helpers for E2E tests

# Exit code 77 = skip (standard autotools convention)
skip_test() {
    echo "[SKIP] $*"
    exit 77
}

# Check if Docker is available
require_docker() {
    if ! docker info &>/dev/null; then
        skip_test "Docker not available"
    fi
}

# Check if SSH agent is available
require_ssh_agent() {
    if [[ -z "${SSH_AUTH_SOCK:-}" ]]; then
        skip_test "SSH_AUTH_SOCK not set"
    fi
    if [[ ! -S "$SSH_AUTH_SOCK" ]]; then
        skip_test "SSH agent socket not found: $SSH_AUTH_SOCK"
    fi
    if ! ssh-add -l &>/dev/null; then
        local exit_code=$?
        # Exit code 1 means agent has no keys, which is fine for basic forwarding test
        # Exit code 2 means agent is not running
        if [[ $exit_code -eq 2 ]]; then
            skip_test "SSH agent not running"
        fi
    fi
}

# Check if GPG agent is available
require_gpg_agent() {
    if ! command -v gpg &>/dev/null; then
        skip_test "gpg not installed"
    fi

    local socket
    socket=$(gpgconf --list-dirs agent-socket 2>/dev/null) || skip_test "gpgconf failed"

    if [[ ! -S "$socket" ]]; then
        skip_test "GPG agent socket not found: $socket"
    fi

    # Check if GPG agent is responsive
    if ! gpg-connect-agent /bye &>/dev/null; then
        skip_test "GPG agent not responsive"
    fi
}

# Check if git credentials are configured
require_git_creds() {
    if ! command -v git &>/dev/null; then
        skip_test "git not installed"
    fi
    # Check if any credential helper is configured
    if ! git config --get-all credential.helper &>/dev/null; then
        skip_test "No git credential helper configured"
    fi
}
