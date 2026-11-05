#!/bin/sh
# Integration test for remote hosts with a real sshd, in Docker.
#   test/remote/run.sh            build, run, and remove the containers
# It covers SSH without install, `algodtop remote install` from a signed
# release directory, the SSH agent with catch-up after the hub was down,
# and the remote terminal UI (-H ssh://).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
work=${WORK:-$(mktemp -d)}
net=algodtop-test-net
arch=$(docker info --format '{{.Architecture}}')
case $arch in aarch64|arm64) goarch=arm64 ;; *) goarch=amd64 ;; esac

cleanup() {
  docker rm -f algodtop-remote1 algodtop-hub >/dev/null 2>&1 || true
  docker network rm $net >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

echo "== build linux/$goarch binaries and a signed release"
mkdir -p "$work/rel" "$work/bin"
(cd "$repo" && go build -o "$work/bin/algodtop-sign" ./internal/release/cmd/algodtop-sign)
pub=$("$work/bin/algodtop-sign" keygen "$work/release.key")
(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath \
  -ldflags "-X algod.top/internal/release.PublicKey=$pub" -o "$work/bin/algodtop" ./algodtop)
(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -o "$work/bin/fakealgod" ./internal/fakealgod/cmd/fakealgod)
cp "$work/bin/algodtop" "$work/rel/algodtop_linux_$goarch"
"$work/bin/algodtop-sign" sign "$work/release.key" "$work/rel" "algodtop_linux_$goarch"

echo "== image and network"
docker build -q -t algodtop-test-ssh "$here" >/dev/null
docker network create $net >/dev/null
ssh-keygen -q -t ed25519 -N '' -f "$work/hubkey"

docker run -d --name algodtop-remote1 --hostname remote1 --network $net -v "$work:/w:ro" algodtop-test-ssh \
  sh -c 'mkdir -p /home/algo/.ssh && cp /w/hubkey.pub /home/algo/.ssh/authorized_keys && chown -R algo /home/algo/.ssh && chmod 700 /home/algo/.ssh &&
         su algo -c "/w/bin/fakealgod -d /home/algo/.mainnet -l 127.0.0.1:8080 -network mainnet -admin-token adm -token tok -round 1s -churn 2s >/tmp/fake.log 2>&1 &" &&
         exec /usr/sbin/sshd -D -e' >/dev/null
docker run -d --name algodtop-hub --hostname hub --network $net -v "$work:/w:ro" algodtop-test-ssh sleep infinity >/dev/null
sleep 2
hub() { docker exec -u hub algodtop-hub sh -c "$1"; }
docker exec algodtop-hub sh -c 'install -d -o hub -m 700 /home/hub/.ssh && install -o hub -m 600 /w/hubkey /home/hub/.ssh/id_ed25519'
for i in $(seq 1 20); do
  if hub 'ssh-keyscan remote1 > ~/.ssh/known_hosts 2>/dev/null && test -s ~/.ssh/known_hosts'; then break; fi
  sleep 1
done
hub 'printf "Host remote1\n  User algo\n" > ~/.ssh/config && mkdir -p ~/bin && cp /w/bin/algodtop ~/bin/ && ssh -o BatchMode=yes remote1 true'

echo "== 1. SSH without install"
hub '~/bin/algodtop -u main=ssh://remote1/~/.mainnet -b -n 1' | tee "$work/noinstall.txt"
grep -q 'main .* 4.4.1' "$work/noinstall.txt"

echo "== 2. remote install from a signed release"
hub 'mkdir -p ~/.config/algodtop && printf "{\"web\":{\"listen\":\"\"},\"nodes\":[]}" > ~/.config/algodtop/config.json && chmod 600 ~/.config/algodtop/config.json'
hub '~/bin/algodtop remote install remote1 --release-dir /w/rel --yes' | tee "$work/install.txt"
docker exec -u algo algodtop-remote1 sh -c 'test -x ~/.local/bin/algodtop && ~/.local/bin/algodtop version && cat ~/.config/algodtop/agent.json'
hub 'grep -q remote1 ~/.config/algodtop/config.json'
echo "   an unsigned binary is refused:"
if hub '~/bin/algodtop remote install remote1 --binary ~/bin/algodtop --yes' 2>&1 | grep -q 'unsigned'; then echo "   ok"; else echo "   FAIL"; exit 1; fi

echo "== 3. SSH agent: run the agent as a service would, pull from the hub"
docker exec -u algo -d algodtop-remote1 sh -c '~/.local/bin/algodtop agent >/tmp/agent.log 2>&1'
hub 'nohup ~/bin/algodtop daemon >/tmp/hub.log 2>&1 &'
for i in $(seq 1 30); do
  if hub '~/bin/algodtop -b' 2>/dev/null | grep -q 'remote1/mainnet .* OK'; then break; fi
  sleep 1
done
hub '~/bin/algodtop -b' | tee "$work/agent.txt"
grep -q 'remote1/mainnet' "$work/agent.txt"

echo "== 4. hub down for 40 s: the agent buffers, the hub catches up with no gap"
hub 'pkill -INT -x algodtop; sleep 3'
down_from=$(date +%s)
sleep 40
hub 'nohup ~/bin/algodtop daemon >>/tmp/hub.log 2>&1 &'
sleep 15
hub "~/bin/algodtop export --node remote1/mainnet --metric algodtop_round --fn last --tier raw --from $((down_from-20)) --to $((down_from+45))" > "$work/catchup.csv"
gap=$(tail -n +2 "$work/catchup.csv" | cut -c12-23 | awk -F: '{ t=$1*3600+$2*60+$3; if (NR>1 && t-p>m) m=t-p; p=t } END { printf "%.1f", m }')
lines=$(tail -n +2 "$work/catchup.csv" | grep -c . || true)
echo "   $lines samples across the outage, largest gap ${gap}s (1 s rounds)"
test "$lines" -ge 40
awk -v g="$gap" 'BEGIN { exit !(g < 3) }'

echo "== 5. remote terminal UI over ssh (-H) against a daemon on remote1"
docker exec -u algo -d algodtop-remote1 sh -c 'mkdir -p ~/.config/algodtop && printf "{\"web\":{\"listen\":\"\"},\"nodes\":[{\"name\":\"mainnet\",\"datadir\":\"~/.mainnet\"}]}" > ~/.config/algodtop/config.json && chmod 600 ~/.config/algodtop/config.json && ~/.local/bin/algodtop daemon >/tmp/daemon.log 2>&1'
sleep 6
hub 'PATH=$PATH:~/bin ~/bin/algodtop -H ssh://remote1/~/.local/bin/algodtop -b' | tee "$work/hubtui.txt"
grep -q 'mainnet' "$work/hubtui.txt"

echo "== all remote tests passed"
