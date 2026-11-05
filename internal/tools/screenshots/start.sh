#!/bin/sh
# The demo container: five fake nodes and an algod.top daemon.
set -e
/fakes.sh mainnet-1 mainnet-relay mainnet-archive testnet localnet
sleep 2
exec algod.top daemon --config /etc/algodtop/config.json
