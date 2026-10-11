#!/usr/bin/env bash
# Same success/failure cases, against the generative model, for the head-to-head.
set -u
MODEL="${1:-qwen-uncensored-32k:latest}"
URL="http://localhost:11434/api/chat"
SYS="You evaluate a command's outcome. Given the command output, decide if the command SUCCEEDED or FAILED at its apparent goal. Answer with exactly one word: SUCCESS or FAILURE. No explanation."

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

ok=0; n=0
for c in "${CASES[@]}"; do
  out="${c%%|||*}"; exp="${c##*|||}"
  body="{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"system\",\"content\":\"$SYS\"},{\"role\":\"user\",\"content\":\"$out\"}],\"stream\":false,\"options\":{\"temperature\":0}}"
  resp=$(curl -s -m 120 "$URL" -d "$body")
  content=$(printf '%s' "$resp" | grep -oE '"content":"[^"]*"' | head -1 | sed 's/"content":"//; s/"$//' | tr '[:upper:]' '[:lower:]')
  n=$((n+1))
  got="?"
  printf '%s' "$content" | grep -q "success" && got="success"
  printf '%s' "$content" | grep -q "failure" && got="failure"
  if [ "$got" = "$exp" ]; then ok=$((ok+1)); st="OK   "; else st="WRONG"; fi
  echo "[$st] exp=$exp got=$got  ${out:0:46}"
done
echo "=== qwen success/failure: ACCURACY $ok/$n ($((100*ok/n))%) | model $MODEL ==="
