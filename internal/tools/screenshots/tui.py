"""Capture the algodtop terminal UI at a fixed size and render it as an image.

usage: tui.py BIN HUB OUT.png [KEYS...]
       tui.py --raw OUT.png.raw OUT.png
KEYS are "SECONDS:TEXT" pairs typed into the UI, for example "4:\r". It
also writes what the UI printed to OUT.png.raw, and --raw renders that
again without running the UI.
"""
import asyncio, fcntl, html, os, pty, select, struct, sys, termios, time

import pyte
from playwright.async_api import async_playwright

COLS, ROWS = 150, 40


def capture(bin_, hub, args):
    keys = []
    for k in args:
        t, s = k.split(":", 1)
        keys.append((float(t), s.encode().decode("unicode_escape").encode()))
    total = (keys[-1][0] if keys else 0) + 4
    pid, fd = pty.fork()
    if pid == 0:
        fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
        os.environ.update(TERM="xterm-256color", COLORTERM="truecolor", TZ="UTC")
        os.execv(bin_, [bin_, "-H", hub, "--utc"])
    data = b""
    start = time.time()
    pending = list(keys)
    while time.time() - start < total:
        if pending and time.time() - start >= pending[0][0]:
            os.write(fd, pending.pop(0)[1])
        r, _, _ = select.select([fd], [], [], 0.05)
        if r:
            try:
                data += os.read(fd, 65536)
            except OSError:
                break
    os.write(fd, b"q")
    time.sleep(0.5)
    try:
        os.kill(pid, 9)
    except ProcessLookupError:
        pass
    return data


if sys.argv[1] == "--raw":
    out = sys.argv[3]
    with open(sys.argv[2], "rb") as f:
        data = f.read()
else:
    out = sys.argv[3]
    data = capture(sys.argv[1], sys.argv[2], sys.argv[4:])
    with open(out + ".raw", "wb") as f:
        f.write(data)

screen = pyte.Screen(COLS, ROWS)
pyte.ByteStream(screen).feed(data)

# A dark terminal palette.
BG, FG = "#141413", "#e6e5df"
NAMED = {
    "black": "#3a3a37", "red": "#e5686b", "green": "#5fc28d", "brown": "#d9a74a", "yellow": "#d9a74a",
    "blue": "#6aa6e8", "magenta": "#c38ae0", "cyan": "#55c1c8", "white": "#d6d5cf",
    "brightblack": "#77766f", "brightred": "#ff8a8c", "brightgreen": "#7fe0a9", "brightbrown": "#f0c36a",
    "brightyellow": "#f0c36a", "brightblue": "#8cc0ff", "brightmagenta": "#d9a8f5", "brightcyan": "#7fdbe0",
    "brightwhite": "#ffffff",
}

def color(c, default):
    if c in ("default", None):
        return default
    if c in NAMED:
        return NAMED[c]
    if len(c) == 6 and all(ch in "0123456789abcdefABCDEF" for ch in c):
        return "#" + c
    return default

rows = []
for y in range(ROWS):
    line = screen.buffer[y]
    spans, cur, text = [], None, ""
    for x in range(COLS):
        ch = line[x]
        fg, bg = color(ch.fg, FG), color(ch.bg, BG)
        if ch.reverse:
            fg, bg = bg, fg
        style = (fg, bg, ch.bold)
        if style != cur and text:
            spans.append((cur, text))
            text = ""
        cur = style
        text += ch.data or " "
    if text:
        spans.append((cur, text))
    rows.append("".join(
        '<span style="color:%s;background:%s;%s">%s</span>' % (s[0], s[1], "font-weight:600;" if s[2] else "", html.escape(t))
        for s, t in spans))

page = """<!doctype html><html><head><meta charset="utf-8"><style>
body { margin: 0; background: %s; }
pre { display: inline-block; margin: 0; padding: 18px 22px; font: 14px/1.32 Menlo, "SF Mono", monospace; color: %s; white-space: pre; }
</style></head><body><pre>%s</pre></body></html>""" % (BG, FG, "\n".join(rows))
open(out + ".html", "w").write(page)


async def shoot():
    async with async_playwright() as p:
        b = await p.chromium.launch(channel="chrome", headless=True)
        # Resizing a 2x page after it loads makes Chrome tile its first
        # quarter, so the viewport starts large and the shot is of the <pre>.
        pg = await b.new_page(viewport={"width": 1600, "height": 1000}, device_scale_factor=2)
        await pg.goto("file://" + os.path.abspath(out + ".html"))
        await pg.locator("pre").screenshot(path=out)
        print("saved", out)
        await b.close()

asyncio.run(shoot())
