#!/bin/sh
# fakes.sh NAME... starts or restarts the named fake nodes. Rounds follow
# the wall clock, so a restarted node carries on where it was. Peer
# addresses come from the RFC 5737 documentation ranges. Traffic rises and
# falls, and moves over real sockets so the wire columns have data.
# TESTNET_VERSION upgrades the testnet node on its next start.
A=/var/lib/algorand
NOW=$(date +%s)
mr() { echo $(( 66612000 + (NOW - 1793745000) * 10 / 28 + $1 )); }
MAIN="-network mainnet -genesis-hash wGHE2Pwdvd7S12BL5FaOP20EGYesN73ktiC1qzkkit8= -branch rel/stable -channel stable"
run() {
  name=$1; shift
  mkdir -p $A/$name
  if [ -f $A/$name/algod.pid ]; then kill "$(cat $A/$name/algod.pid)" 2>/dev/null; sleep 1; fi
  nohup /usr/bin/algod -d $A/$name -token "tok-$name" -admin-token "adm-$name" -doc-ips -peer-usage -trace 5 -jitter -wave -wire -uptime 1m "$@" > /tmp/$name.out 2>&1 &
}
for n in "$@"; do
  case $n in
  mainnet-1)
    [ -f $A/mainnet-1/config.json ] || { mkdir -p $A/mainnet-1; printf '{\n  "EnableP2PHybridMode": true,\n  "GossipFanout": 4\n}\n' > $A/mainnet-1/config.json; }
    run mainnet-1 $MAIN -first-round $(mr 0) -version 5.0.5 -peers 22 -churn 2m -load 1.0 \
      -account demo-participation-a:250000:94 -account demo-participation-b:42000:5 ;;
  mainnet-relay) run mainnet-relay $MAIN -first-round $(mr 3) -version 5.0.5 -peers 96 -churn 15s -load 4.6 ;;
  mainnet-archive) run mainnet-archive $MAIN -first-round $(mr -2) -version 5.0.4 -peers 14 -churn 3m -load 0.7 ;;
  testnet) run testnet -network testnet -genesis-hash SGO1GKSzyE7IEPItTxCByw9x8FmnrCDexi9/cOUJOiI= -branch rel/beta -channel beta \
      -first-round $(( 61873000 + (NOW - 1793745000) * 10 / 28 )) -version ${TESTNET_VERSION:-5.1.1} -peers 18 -churn 2m -load 0.3 ;;
  localnet) run localnet -network localnet -genesis-hash Q/OH6KUDmYNH+TbYWWo/GU4Y6ZkL/giqux8wfqW8YC4= -branch master -channel dev \
      -first-round $(( 1240 + NOW - 1793745000 )) -version 5.2.0 -peers 6 -churn 5m -round 1s -load 0.02 ;;
  esac
done
