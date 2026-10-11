#!/usr/bin/env bash
# Benchmark the zero-shot classifier on PentAGI's "Docker image selection" task.
# Usage: bench_images.sh [classifier_url]
set -u
URL="${1:-http://localhost:8077/classify}"

LABELS='["penetration testing and security tools","Python programming","Node.js and JavaScript development","Go programming","general purpose Linux shell commands"]'

declare -A MAP=(
  ["penetration testing and security tools"]="vxcontrol/kali-linux"
  ["Python programming"]="python:latest"
  ["Node.js and JavaScript development"]="node:latest"
  ["Go programming"]="golang:latest"
  ["general purpose Linux shell commands"]="debian:latest"
)

# "task|||expected_image"
CASES=(
"scan the host 10.0.0.5 for open ports and find exploitable services|||vxcontrol/kali-linux"
"write a python script that parses a CSV file and computes column averages|||python:latest"
"list the files in the current directory and check disk usage|||debian:latest"
"compile and run this Go program|||golang:latest"
"start a node.js express web server listening on port 3000|||node:latest"
"use hydra to brute force the SSH login of the target|||vxcontrol/kali-linux"
"run a quick bash one-liner to grep errors from a log file|||debian:latest"
"build a React frontend application with npm|||node:latest"
"exploit a SQL injection vulnerability and dump the database|||vxcontrol/kali-linux"
"create a virtualenv, install pip packages and run a flask app|||python:latest"
"cross-compile a static binary with go build for linux amd64|||golang:latest"
"perform an nmap service-version scan then launch metasploit|||vxcontrol/kali-linux"
)

ok=0; n=0; sumlat=0
for c in "${CASES[@]}"; do
  task="${c%%|||*}"; exp="${c##*|||}"
  body="{\"text\":\"$task\",\"labels\":$LABELS}"
  resp=$(curl -s -m 60 "$URL" -H "Content-Type: application/json" -d "$body")
  label=$(printf '%s' "$resp" | grep -oE '"label":"[^"]*"' | head -1 | sed 's/"label":"//; s/"$//')
  lat=$(printf '%s' "$resp" | grep -oE '"latency_ms":[0-9]+' | head -1 | grep -oE '[0-9]+')
  img="${MAP[$label]:-UNKNOWN}"
  n=$((n+1))
  [ -n "${lat:-}" ] && sumlat=$((sumlat + lat))
  if [ "$img" = "$exp" ]; then ok=$((ok+1)); st="OK   "; else st="WRONG"; fi
  echo "[$st] ${task:0:55} -> $img (exp $exp) ${lat:-?}ms"
done
avg=0; [ "$n" -gt 0 ] && avg=$((sumlat / n))
echo "=== ACCURACY: $ok/$n ($((100*ok/n))%) | avg latency: ${avg}ms ==="
