#!/usr/bin/env bash
# Same image-selection cases, but against the generative model (qwen) using
# PentAGI's real "Docker Image Selector" prompt. For a fair head-to-head.
set -u
MODEL="${1:-qwen-uncensored-32k:latest}"
URL="http://localhost:11434/api/chat"

SYS="You are a precise Docker Image Selector that determines the optimal container environment for execution.\nSelect the most appropriate Docker image for running the user's task, outputting ONLY the image name in lowercase, nothing else.\nGuidelines:\n- Choose based on the required technology stack.\n- Always use latest tags (e.g. python:latest).\n- For security/penetration-testing tasks, default to vxcontrol/kali-linux.\n- For ambiguous cases use debian:latest.\n- Available images: debian:latest, vxcontrol/kali-linux, python:latest, node:latest, golang:latest.\n- Output ONLY the image name, no explanation."

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

ok=0; clean=0; n=0; sumlat=0
for c in "${CASES[@]}"; do
  task="${c%%|||*}"; exp="${c##*|||}"
  body="{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"system\",\"content\":\"$SYS\"},{\"role\":\"user\",\"content\":\"$task\"}],\"stream\":false,\"options\":{\"temperature\":0}}"
  resp=$(curl -s -m 120 "$URL" -d "$body")
  content=$(printf '%s' "$resp" | grep -oE '"content":"[^"]*"' | head -1 | sed 's/"content":"//; s/"$//')
  ed=$(printf '%s' "$resp" | grep -oE '"eval_duration":[0-9]+' | head -1 | grep -oE '[0-9]+')
  lat="?"; [ -n "${ed:-}" ] && lat=$(awk -v d="$ed" 'BEGIN{printf "%.1f", d/1000000000}')
  low=$(printf '%s' "$content" | tr '[:upper:]' '[:lower:]')
  n=$((n+1))
  if printf '%s' "$low" | grep -qF "$exp"; then ok=$((ok+1)); st="OK   "; else st="WRONG"; fi
  # "clean" = output is essentially just the image name (<= 40 chars, no sentence)
  clen=${#content}
  if [ "$clen" -le 40 ]; then clean=$((clean+1)); cl="clean"; else cl="rambled(${clen}c)"; fi
  echo "[$st|$cl] ${task:0:48} -> '${content:0:45}' ${lat}s"
done
echo "=== qwen: ACCURACY $ok/$n ($((100*ok/n))%) | CLEAN-OUTPUT $clean/$n | model $MODEL ==="
