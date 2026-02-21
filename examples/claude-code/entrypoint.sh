#!/usr/bin/env bash
set -euo pipefail

is_repo_slug() {
    [[ "$1" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]]
}

extract_repo_slug() {
    local value="${1%.git}"
    value="${value#git@github.com:}"
    value="${value#https://github.com/}"
    value="${value#http://github.com/}"
    value="${value#ssh://git@github.com/}"
    value="${value#git://github.com/}"
    if is_repo_slug "$value"; then
        printf '%s\n' "$value"
        return 0
    fi
    return 1
}

repo=""
if [ "$#" -gt 0 ] && is_repo_slug "$1"; then
    repo="$1"
    shift
fi

instruction="$*"
if [ -z "$repo" ]; then
    remote_url="$(git -C /workspace remote get-url origin 2>/dev/null || true)"
    if [ -z "$remote_url" ]; then
        echo "Unable to detect repository from /workspace git remote; pass owner/repo explicitly." >&2
        exit 1
    fi
    repo="$(extract_repo_slug "$remote_url" || true)"
    if [ -z "$repo" ]; then
        echo "Unsupported git remote URL: $remote_url" >&2
        exit 1
    fi
fi

if [ -z "${GH_TOKEN:-}" ]; then
    GH_TOKEN="$(gh auth token 2>/dev/null || true)"
    export GH_TOKEN
fi

if [ -z "${GH_TOKEN:-}" ]; then
    echo "GH_TOKEN is required. Set it or run 'gh auth login' before starting." >&2
    exit 1
fi

if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
    echo "ANTHROPIC_API_KEY is required." >&2
    exit 1
fi

claude_settings="$HOME/.claude/settings.json"
if [ ! -f "$claude_settings" ]; then
    mkdir -p "$(dirname "$claude_settings")"
    cat >"$claude_settings" <<'EOF'
{
  "apiKeyHelper": "echo $ANTHROPIC_API_KEY"
}
EOF
fi
chmod 600 "$claude_settings"

gh_bin="$(command -v gh)"
set_git_helper() {
    local host="$1"
    local key="credential.https://${host}.helper"
    git config --global --unset-all "$key" || true
    git config --global --add "$key" "!${gh_bin} auth git-credential"
}
set_git_helper github.com
set_git_helper gist.github.com
if [ -n "${GIT_USER_EMAIL:-}" ]; then
    git config --global user.email "$GIT_USER_EMAIL"
fi
if [ -n "${GIT_USER_NAME:-}" ]; then
    git config --global user.name "$GIT_USER_NAME"
fi
if [ -n "${GIT_EDITOR:-}" ]; then
    git config --global core.editor "$GIT_EDITOR"
fi

repo_dir="/home/agent/workspace/${repo##*/}"
rm -rf "$repo_dir"
if ! git clone --depth=1 "https://github.com/${repo}.git" "$repo_dir"; then
    echo "Authenticated clone failed. Ensure GH_TOKEN can access ${repo}." >&2
    exit 1
fi
cd "$repo_dir"

if [ -z "$instruction" ]; then
    exec claude --dangerously-skip-permissions
fi

exec claude --dangerously-skip-permissions -p "$instruction"
