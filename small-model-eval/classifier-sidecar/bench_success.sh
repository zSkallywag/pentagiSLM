#!/usr/bin/env bash
# Benchmark the zero-shot classifier on success/failure detection of command output.
# Usage: bench_success.sh [classifier_url]
set -u
URL="${1:-http://localhost:8077/classify}"
LABELS='["success","failure"]'
HYP="This command output indicates a {}."

# "command output (single line)|||expected"
CASES=(
"student@target:~$ whoami -> student|||success"
"ssh: connect to host 192.168.1.0 port 22: Connection refused|||failure"
"Permission denied (publickey,password).|||failure"
"[22][ssh] host: 192.168.1.0 login: student password: letmein - 1 valid password found|||success"
"hydra finished: 0 valid passwords found|||failure"
"cat: /root/flag: Permission denied|||failure"
"sudo: a terminal is required to read the password|||failure"
"bash: nmap: command not found|||failure"
"PORT STATE SERVICE - 22/tcp open ssh|||success"
"HTTP/1.1 200 OK|||success"
"HTTP/1.1 401 Unauthorized|||failure"
"root@target:/# id -> uid=0(root) gid=0(root) groups=0(root)|||success"
)

ok=0; n=0; sumlat=0
for c in "${CASES[@]}"; do
  out="${c%%|||*}"; exp="${c##*|||}"
  body="{\"text\":\"$out\",\"labels\":$LABELS,\"hypothesis_template\":\"$HYP\"}"
  resp=$(curl -s -m 60 "$URL" -H "Content-Type: application/json" -d "$body")
  label=$(printf '%s' "$resp" | grep -oE '"label":"[^"]*"' | head -1 | sed 's/"label":"//; s/"$//')
  lat=$(printf '%s' "$resp" | grep -oE '"latency_ms":[0-9]+' | head -1 | grep -oE '[0-9]+')
  n=$((n+1)); [ -n "${lat:-}" ] && sumlat=$((sumlat + lat))
  if [ "$label" = "$exp" ]; then ok=$((ok+1)); st="OK   "; else st="WRONG"; fi
  echo "[$st] exp=$exp got=$label  ${out:0:48}  ${lat:-?}ms"
done
avg=0; [ "$n" -gt 0 ] && avg=$((sumlat / n))
echo "=== classifier success/failure: $ok/$n ($((100*ok/n))%) | avg ${avg}ms ==="
