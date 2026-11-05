# Screenshots

`run.sh` makes the screenshots on https://algod.top from a demo fleet in Docker. The fleet is five fake nodes from `internal/fakealgod`:

| Node | Shows |
|---|---|
| `mainnet-1` | A participation node with two online accounts, one of whose keys expires in 5 days, and a config change that takes effect at a restart |
| `mainnet-relay` | A relay with 96 peers and a planned restart under a silence |
| `mainnet-archive` | One patch release behind, which raises the version warning |
| `testnet` | The beta channel, upgraded during the run |
| `localnet` | A private network with 1 s rounds on a developer build, down for four minutes |

Peer addresses come from the RFC 5737 documentation ranges, and the accounts are well-formed addresses with no known key. Traffic rises and falls over the hours, and each fake moves its traffic over loopback sockets, so the wire columns have data.

```
internal/tools/screenshots/run.sh start   # build and start the demo on 127.0.0.1:18090
internal/tools/screenshots/story.sh       # six hours of events, then write site/img/*.webp
internal/tools/screenshots/run.sh stop
```

`story.sh` lists what happens when. To shoot sooner, run `RANGE=1h internal/tools/screenshots/run.sh shoot` after an hour. To restart a node by hand:

```
docker exec -d algodtop-demo /fakes.sh mainnet-relay
```

The web pages are taken in light and dark at 2x. The terminal UI runs on the host in a pseudo-terminal against the demo daemon, and `pyte` renders its screen. It needs Docker, Go, uv and Google Chrome.

## Shooting again without the demo

`shoot` records what the shots show in `saved/TIME/`, which git ignores, and points `saved/latest` at it. The browser talks to a small server in `shoot.py`. That server serves the page, JS and CSS from `internal/web/static`, passes each API request to the daemon, and saves the answer. Each shot runs at a fixed browser clock, and the recording keeps that clock. The terminal UI's output goes in `tui.raw`.

```
internal/tools/screenshots/run.sh shoot     # after story.sh, or at any time
internal/tools/screenshots/run.sh save      # stop the demo, archive its store
internal/tools/screenshots/run.sh stop
internal/tools/screenshots/run.sh reshoot   # later: site/img from saved/latest
```

`reshoot` needs no Docker. It replays the saved answers at the saved clocks with the current web UI, so a change to the page, JS or CSS shows in a minute. Two replays give the same pixels. A request that the recording lacks gets the nearest saved answer for the same path, and the script names it. A change to the daemon's answers, or to the terminal UI, needs a new demo. `FROM=internal/tools/screenshots/saved/TIME` picks an older recording.

`save` adds `store.tar.gz`, the daemon's store, and `algorand.tar.gz`, the fake nodes' data directories, to the recording. The history in them ends when the demo stopped.
