#!/usr/bin/env bash
set -euo pipefail

# scripts/quickstart.sh - 10-second local onboarding for llm-proxy

echo "=========================================================="
echo "           🚀 Welcome to llm-proxy Quickstart            "
echo "=========================================================="
echo ""

# 1. Verify Docker and Compose
if ! command -v docker >/dev/null 2>&1; then
  echo "❌ Error: docker is not installed or not in PATH."
  exit 1
fi

if ! docker compose version >/dev/null 2>&1; then
  echo "❌ Error: 'docker compose' plugin is required."
  exit 1
fi

# 2. Check or prompt for vendor API key
if [[ -z "${OPENAI_API_KEY:-}" && -z "${GEMINI_API_KEY:-}" && -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "No vendor API keys detected in your environment."
  echo "You can pass your OpenAI API key now (or press Enter to skip and connect local nodes later):"
  read -r -p "Enter OPENAI_API_KEY (optional): " user_key || true
  if [[ -n "${user_key:-}" ]]; then
    export OPENAI_API_KEY="$user_key"
  fi
fi

# 3. Create .env with generated secrets if not present
if [[ ! -f ".env" ]]; then
  echo "📝 Generating fresh local .env with unique encryption keys..."
  RAND_KEY="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
  RAND_BOOTSTRAP="$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | tr -d '\n')"
  
  cat <<EOF > .env
DATABASE_URL=postgres://llmproxy:llmproxy@db:5432/llmproxy?sslmode=disable
LLMPROXY_ENCRYPTION_KEYS=1:${RAND_KEY}
BOOTSTRAP_ADMIN_KEY=${RAND_BOOTSTRAP}
AUTO_SEED=true
SEED_CLIENT_KEY=llm_dev_default_key
OPENAI_API_KEY=${OPENAI_API_KEY:-}
GEMINI_API_KEY=${GEMINI_API_KEY:-}
ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY:-}
EOF
  chmod 600 .env
fi

# 4. Start Docker Compose
echo "🐳 Starting llm-proxy, PostgreSQL, and Redis containers..."
docker compose up -d

# 5. Wait for proxy to become ready
echo "⏳ Waiting for llm-proxy to become ready on http://localhost:8080/readyz..."
ready=0
for i in {1..30}; do
  if curl -s -f "http://localhost:8080/readyz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done

if [[ "$ready" -ne 1 ]]; then
  echo "⚠️ Proxy took longer than expected to start. Check logs with: docker compose logs llm-proxy"
  exit 1
fi

echo ""
echo "=========================================================="
echo "✅ llm-proxy is up and ready to serve!"
echo "=========================================================="
echo ""
echo "Test your first completion:"
echo ""
echo 'curl -s http://localhost:8080/v1/chat/completions \'
echo '  -H "Authorization: Bearer llm_dev_default_key" \'
echo '  -H "Content-Type: application/json" \'
echo '  -d '\''{"model":"auto","messages":[{"role":"user","content":"Say hi in three words."}]}'\'
echo ""
echo "Or using Python:"
echo ""
echo 'from openai import OpenAI'
echo 'client = OpenAI(base_url="http://localhost:8080/v1", api_key="llm_dev_default_key")'
echo 'print(client.chat.completions.create(model="auto", messages=[{"role":"user","content":"Say hi!"}]).choices[0].message.content)'
echo ""
