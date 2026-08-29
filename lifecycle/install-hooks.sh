#!/bin/sh
# Install the secret-scan pre-commit hook.  Re-run after a fresh clone.
set -eu
cd "$(dirname "$0")/.."
cat > .git/hooks/pre-commit <<'HOOK'
#!/bin/sh
git diff --cached | bash lifecycle/presubmit-check.sh
HOOK
chmod +x .git/hooks/pre-commit
echo "pre-commit hook installed"
