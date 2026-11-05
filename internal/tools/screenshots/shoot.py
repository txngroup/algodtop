"""Screenshot web UI pages of the demo daemon in light and dark.

usage: shoot.py [--record DIR | --replay DIR] OUTDIR NAME=PATH...

The browser talks to a small server in this script. It serves the page,
its JS and its CSS from internal/web/static, so changes show without
rebuilding the demo, and it passes /api/ requests to the daemon. Each
shot runs at a fixed browser clock. --record saves every API answer and
each shot's clock in DIR. --replay answers from DIR at the saved clocks
and needs no daemon, so later UI changes can be shot against the same
data.
"""
import asyncio, gzip, http.client, http.server, json, os, sys, threading, time, urllib.parse

from playwright.async_api import async_playwright

BASE = os.environ.get("DEMO_URL", "http://127.0.0.1:18090")
STATIC = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "web", "static"))
TYPES = {".css": "text/css", ".js": "text/javascript", ".html": "text/html; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2"}

mode, recdir = None, None
args = sys.argv[1:]
if args and args[0] in ("--record", "--replay"):
    mode, recdir, args = args[0][2:], args[1], args[2:]
out, pages = args[0], [tuple(a.split("=", 1)) for a in args[1:]]

# rec maps a scope, such as "light/fleet", to {request key: answer}. An
# answer is {"status", "type", "body"}, or {"sse": [[seconds, message],
# ...]} for the live stream, with each message's time after the stream
# opened.
rec, clocks, lock = {}, {}, threading.Lock()
scope = [""]
# sealed holds the shots already taken. Answers that come after a shot
# never reach the recording, so the replay shows what the shot showed.
sealed = set()


def keep(sc, key, answer):
    with lock:
        if mode == "record" and sc not in sealed:
            rec.setdefault(sc, {})[key] = answer


def key_of(path):
    u = urllib.parse.urlsplit(path)
    q = sorted(urllib.parse.parse_qsl(u.query, keep_blank_values=True))
    return u.path + ("?" + urllib.parse.urlencode(q) if q else "")


def num(v):
    try:
        return float(v)
    except ValueError:
        return None


def closest(key):
    """Finds the recorded answer for key, or the nearest one: the same path
    and words, with the nearest numbers. It prefers the current shot."""
    order = [scope[0]] + [s for s in rec if s != scope[0]]
    for s in order:
        if key in rec.get(s, {}):
            return rec[s][key], s == scope[0]
    path, _, query = key.partition("?")
    want = dict(urllib.parse.parse_qsl(query, keep_blank_values=True))
    best, cost = None, None
    for s in order:
        for k, a in rec.get(s, {}).items():
            p, _, q = k.partition("?")
            have = dict(urllib.parse.parse_qsl(q, keep_blank_values=True))
            if p != path or have.keys() != want.keys():
                continue
            c = 0.0
            for name, v in want.items():
                x, y = num(v), num(have[name])
                if x is None or y is None:
                    if v != have[name]:
                        break
                else:
                    c += abs(x - y) / max(1.0, abs(x))
            else:
                if cost is None or c < cost:
                    best, cost = a, c
    return best, False


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, request, addr):
        # The browser drops connections when it leaves a page.
        if not isinstance(sys.exc_info()[1], OSError):
            super().handle_error(request, addr)


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def reply(self, status, ctype, body):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?")[0]
        if path.startswith("/api/") or path == "/metrics":
            return self.api()
        # Page routes all load the single-page app, as in the daemon.
        if "." not in path[1:]:
            path = "/index.html"
        f = os.path.normpath(STATIC + path)
        if not f.startswith(STATIC + os.sep) or not os.path.isfile(f):
            return self.reply(404, "text/plain", b"not found")
        with open(f, "rb") as fh:
            self.reply(200, TYPES.get(os.path.splitext(f)[1], "application/octet-stream"), fh.read())

    def api(self):
        key, sc = key_of(self.path), scope[0]
        if mode == "replay":
            a, here = closest(key)
            if a is None:
                print("replay: no answer for", key, file=sys.stderr)
                return self.reply(404, "application/json", b'{"error":"not recorded"}')
            if not here:
                print("replay: borrowed an answer for", key, file=sys.stderr)
            if "sse" in a:
                return self.replay_stream(a["sse"])
            return self.reply(a["status"], a["type"], a["body"].encode())
        u = urllib.parse.urlsplit(BASE)
        c = http.client.HTTPConnection(u.hostname, u.port, timeout=30)
        c.request("GET", self.path, headers={"Accept-Encoding": "identity", "Accept": self.headers.get("Accept", "*/*")})
        r = c.getresponse()
        ctype = r.getheader("Content-Type", "application/octet-stream")
        if ctype.startswith("text/event-stream"):
            return self.pass_stream(c, r, key, sc)
        body = r.read()
        c.close()
        keep(sc, key, {"status": r.status, "type": ctype, "body": body.decode()})
        self.reply(r.status, ctype, body)

    def stream_head(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-store")
        self.end_headers()

    def pass_stream(self, c, r, key, sc):
        """Passes the live stream through and records its messages."""
        self.stream_head()
        msgs, cur, t0 = [], b"", time.monotonic()
        try:
            while True:
                line = r.readline()
                if not line:
                    break
                self.wfile.write(line)
                cur += line
                if line in (b"\n", b"\r\n"):
                    self.wfile.flush()
                    msgs.append([round(time.monotonic() - t0, 3), cur.decode()])
                    cur = b""
                    keep(sc, key, {"sse": list(msgs)})
        except OSError:
            pass
        finally:
            c.close()

    def replay_stream(self, msgs):
        """Sends the recorded messages at their recorded times, since a page
        may render on its own timer, then holds the stream open as the
        daemon would, so the page never sees it drop."""
        self.stream_head()
        t0 = time.monotonic()
        try:
            for dt, m in msgs:
                time.sleep(max(0, t0 + dt - time.monotonic()))
                self.wfile.write(m.encode())
                self.wfile.flush()
            while True:
                time.sleep(1)
                self.wfile.write(b": still here\n\n")
                self.wfile.flush()
        except OSError:
            pass


async def main():
    global rec, clocks
    if mode == "replay":
        with gzip.open(os.path.join(recdir, "api.json.gz"), "rt") as f:
            saved = json.load(f)
        rec, clocks = saved["answers"], saved["clocks"]
    srv = Server(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    base = "http://127.0.0.1:%d" % srv.server_address[1]
    async with async_playwright() as p:
        b = await p.chromium.launch(channel="chrome", headless=True)
        for scheme in ("light", "dark"):
            ctx = await b.new_context(viewport={"width": 1280, "height": 800}, device_scale_factor=2,
                                      color_scheme=scheme, locale="en-US", timezone_id="UTC")
            pg = await ctx.new_page()
            for name, path in pages:
                sc = scheme + "/" + name
                if mode == "replay":
                    t = clocks.get(sc) or max(clocks.values())
                    if sc not in clocks:
                        print("replay: no clock for", sc, "so it uses the latest", file=sys.stderr)
                else:
                    t = int(time.time() * 1000)
                    clocks[sc] = t
                scope[0] = sc
                await pg.clock.set_fixed_time(t / 1000)
                await pg.goto(base + path, wait_until="commit")
                await pg.wait_for_timeout(5000)
                with lock:
                    sealed.add(sc)
                dst = os.path.join(out, f"{name}-{scheme}.png")
                await pg.screenshot(path=dst, animations="disabled")
                print("saved", dst)
            await ctx.close()
        await b.close()
    srv.shutdown()
    if mode == "record":
        os.makedirs(recdir, exist_ok=True)
        tmp = os.path.join(recdir, "api.json.gz.tmp")
        with lock, gzip.open(tmp, "wt") as f:
            json.dump({"answers": rec, "clocks": clocks}, f)
        os.replace(tmp, os.path.join(recdir, "api.json.gz"))
        print("recorded", sum(len(v) for v in rec.values()), "answers in", recdir)


asyncio.run(main())
