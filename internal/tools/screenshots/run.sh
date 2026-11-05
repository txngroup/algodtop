#!/bin/sh
# Regenerates the screenshots in site/img from a demo fleet in Docker:
# three Mainnet nodes, a Testnet node and a local network, all fake, with
# peer addresses from the documentation ranges.
#
#   internal/tools/screenshots/run.sh start    build and start the demo
#   internal/tools/screenshots/run.sh shoot    take the screenshots, after a
#                                               few hours of history, and
#                                               record what they show
#   internal/tools/screenshots/run.sh reshoot  take them again from the
#                                               recording, with no demo
#   internal/tools/screenshots/run.sh save     stop the demo and add its
#                                               store and node directories
#                                               to the recording
#   internal/tools/screenshots/run.sh stop     remove the demo and its data
#
# Recordings go in internal/tools/screenshots/saved/TIME, which git
# ignores, and saved/latest names the newest. FROM picks another for
# reshoot. It needs Docker, Go, uv and Google Chrome.
set -e
cd "$(dirname "$0")/../../.."
T=internal/tools/screenshots
NAME=algodtop-demo

# web MODE DIR OUT RANGE shoots the web pages, recording or replaying.
web() {
	uv run --no-project --with playwright python -I $T/shoot.py "--$1" "$2" "$3" \
		'fleet=/' "node=/node/mainnet-1?r=$4"
}

# webp OUT converts the shots in OUT to site/img.
webp() {
	mkdir -p site/img
	uv run --no-project --with pillow python -I - "$1" site/img <<'EOF'
import glob, os, sys
from PIL import Image
src, dst = sys.argv[1], sys.argv[2]
for p in sorted(glob.glob(os.path.join(src, "*.png"))):
    name = os.path.splitext(os.path.basename(p))[0] + ".webp"
    im = Image.open(p)
    im.save(os.path.join(dst, name), "WEBP", quality=88, method=6)
    print(name, im.size)
EOF
}

case "$1" in
start)
	arch=$(docker info --format '{{.Architecture}}')
	case $arch in aarch64|arm64) arch=arm64 ;; *) arch=amd64 ;; esac
	# The image builds from a temporary directory, so no binary lands in
	# the source tree.
	ctx=$(mktemp -d)
	cp $T/Dockerfile $T/config.json $T/start.sh $T/fakes.sh "$ctx"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$ctx/algodtop" ./algodtop
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$ctx/algod" ./internal/fakealgod/cmd/fakealgod
	docker build -q -t $NAME "$ctx"
	rm -rf "$ctx"
	docker volume create $NAME-store >/dev/null
	docker run --rm -u root -v $NAME-store:/s $NAME chown algorand:algorand /s
	docker run -d --name $NAME --hostname algod-host -p 127.0.0.1:18090:8090 -v $NAME-store:/var/lib/algodtop $NAME
	echo "demo on http://127.0.0.1:18090; restart a node with: docker exec -d $NAME /fakes.sh NAME"
	;;
shoot)
	R=${RANGE:-6h} # the chart range, such as 1h or 6h
	stamp=$(date -u +%Y%m%dT%H%M%SZ)
	rec=$T/saved/$stamp
	mkdir -p "$rec"
	echo "$R" > "$rec/range"
	git rev-parse HEAD > "$rec/commit"
	out=$(mktemp -d)
	go build -o "$out/algodtop" ./algodtop
	web record "$rec" "$out" "$R"
	uv run --no-project --with pyte --with playwright python -I $T/tui.py "$out/algodtop" http://127.0.0.1:18090 "$out/tui.png"
	cp "$out/tui.png.raw" "$rec/tui.raw"
	ln -sfn "$stamp" $T/saved/latest
	webp "$out"
	rm -rf "$out"
	echo "recorded in $rec"
	;;
reshoot)
	rec=${FROM:-$T/saved/latest}
	out=$(mktemp -d)
	web replay "$rec" "$out" "$(cat "$rec/range")"
	uv run --no-project --with pyte --with playwright python -I $T/tui.py --raw "$rec/tui.raw" "$out/tui.png"
	webp "$out"
	rm -rf "$out"
	;;
save)
	rec=${FROM:-$T/saved/latest}
	[ -f "$rec/api.json.gz" ] || { echo "no recording in $rec; run shoot first" >&2; exit 1; }
	docker stop $NAME >/dev/null
	docker run --rm -v $NAME-store:/s:ro $NAME tar czf - -C /s --exclude=./algodtop.sock --exclude=./LOCK . > "$rec/store.tar.gz"
	docker cp $NAME:/var/lib/algorand - | gzip > "$rec/algorand.tar.gz"
	echo "saved the store and node directories in $rec; run.sh stop removes the demo"
	;;
stop)
	docker rm -f $NAME >/dev/null 2>&1 || true
	docker volume rm $NAME-store >/dev/null 2>&1 || true
	;;
*)
	echo "usage: $0 start|shoot|reshoot|save|stop" >&2
	exit 2
	;;
esac
