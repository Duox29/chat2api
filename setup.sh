#!/usr/bin/env bash
# chat2api one-click setup (Linux/macOS, bash)
# Installs: npm deps, Playwright Chromium, Go binary, DeepSeek login.
set -u
cd "$(dirname "$0")"

say()  { printf '\033[1;32m[setup]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[setup]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[setup] ERROR:\033[0m %s\n' "$*"; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "'$1' not found. Install it first: $2"; }

say "1/5 checking prerequisites (node, npm, go)..."
need node "https://nodejs.org (LTS >= 18)"
need npm  "comes with Node.js"
need go   "https://go.dev/dl (go >= 1.22)"
node --version; go version

say "2/5 installing npm dependencies..."
npm install || die "npm install failed"

say "3/5 installing headless Chromium (Playwright, one-time download)..."
npx --yes playwright-core install chromium || die "chromium download failed"

say "4/5 checking account.json..."
if [ ! -f account.json ]; then
  cp account.example.json account.json
  warn "created account.json from example - EDIT IT with your DeepSeek email/password, then re-run $0"
  exit 0
fi

say "5/5 building Go gateway..."
go build -o chat2api . || die "go build failed"

say "logging in via headless browser (one-time, may take ~30s)..."
if ! node scripts/login.js; then
  die "login failed (wrong password / captcha / rate-limit). Fix account.json and re-run."
fi

say "DONE. Start the gateway with:"
echo "    PORT=8081 ./chat2api"
echo "  test: curl localhost:8081/health && curl localhost:8081/v1/models"
