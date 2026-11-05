#!/bin/sh
# story.sh plays six hours of fleet life against the demo, then takes the
# screenshots. Start it right after run.sh start. Along the way:
#
#   0:55  a silence and a planned restart of mainnet-relay
#   1:50  a config change on mainnet-1, pending until its restart at 2:20
#   3:00  localnet stops for four minutes, which fires and clears an alert
#   4:00  a silence and an upgrade of testnet to 5.1.2
#   6:05  run.sh shoot
set -e
T=$(cd "$(dirname "$0")" && pwd)
NAME=algodtop-demo
start=$(date +%s)
at() {
	t=$((start + $1 * 60))
	n=$(date +%s)
	if [ "$t" -gt "$n" ]; then sleep $((t - n)); fi
}
x() { docker exec $NAME "$@"; }
at 55
x algodtop silence --node mainnet-relay --for 15m "planned restart: kernel update"
at 56
docker exec -d $NAME /fakes.sh mainnet-relay
at 110
x sed -i 's/"GossipFanout": 4/"GossipFanout": 6/' /var/lib/algorand/mainnet-1/config.json
at 140
docker exec -d $NAME /fakes.sh mainnet-1
at 180
x sh -c 'kill "$(cat /var/lib/algorand/localnet/algod.pid)"'
at 184
docker exec -d $NAME /fakes.sh localnet
at 240
x algodtop silence --node testnet --for 20m "upgrade to 5.1.2"
at 241
docker exec -d -e TESTNET_VERSION=5.1.2 $NAME /fakes.sh testnet
at 365
RANGE=6h "$T/run.sh" shoot
