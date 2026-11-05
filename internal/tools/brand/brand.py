"""Render the favicon sizes and the social card from their sources.

    uv run --no-project --with playwright --with pillow python -I internal/tools/brand/brand.py

site/favicon.svg is the source of the icon. This writes favicon.ico
(16, 32 and 48 px, each drawn by Chrome at its own size so it stays
sharp) and a 180 px apple-touch-icon.png to site/ and to the web UI's
static files, and copies the SVG there too. It also draws the 1280x640
social card in site/img/social.png from site/img/fleet-dark.webp, with
macOS's Menlo and Helvetica Neue. It needs Google Chrome.
"""

import base64
import io
import os
import shutil
import sys

from PIL import Image, ImageDraw, ImageFilter, ImageFont
from playwright.sync_api import sync_playwright

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))
SITE = os.path.join(ROOT, "site")
STATIC = os.path.join(ROOT, "internal", "web", "static")
BG, FG, MUTED, ACCENT, LINE = (20, 20, 19), (236, 235, 230), (163, 162, 156), (79, 197, 154), (46, 46, 43)


def render(page, svg, size):
    """Chrome's drawing of the SVG at size x size, as an RGBA image."""
    uri = "data:image/svg+xml;base64," + base64.b64encode(svg.encode()).decode()
    page.set_content(f'<html><body style="margin:0;background:transparent"><img src="{uri}" width="{size}" height="{size}" style="display:block"></body></html>')
    png = page.locator("img").screenshot(omit_background=True)
    return Image.open(io.BytesIO(png)).convert("RGBA")


def icons():
    svg = open(os.path.join(SITE, "favicon.svg")).read()
    # iOS rounds the corners of a home screen icon itself.
    square = svg.replace('rx="7" fill="#141413"', 'fill="#141413"')
    with sync_playwright() as p:
        browser = p.chromium.launch(channel="chrome")
        page = browser.new_page(viewport={"width": 600, "height": 600})
        small = [render(page, svg, s) for s in (16, 32, 48)]
        touch = render(page, square, 512).resize((180, 180), Image.LANCZOS)
        browser.close()
    for d in (SITE, STATIC):
        small[-1].save(os.path.join(d, "favicon.ico"), sizes=[(16, 16), (32, 32), (48, 48)], append_images=small[:-1])
        touch.convert("RGB").save(os.path.join(d, "apple-touch-icon.png"), optimize=True)
    shutil.copyfile(os.path.join(SITE, "favicon.svg"), os.path.join(STATIC, "favicon.svg"))


def social():
    W, H = 1280, 640
    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)
    mono = ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", 104, index=1)
    sans = ImageFont.truetype("/System/Library/Fonts/HelveticaNeue.ttc", 34, index=0)
    small = ImageFont.truetype("/System/Library/Fonts/Menlo.ttc", 26, index=1)
    x, y = 72, 78
    d.text((x, y), "algod", font=mono, fill=FG)
    d.text((x + d.textlength("algod", font=mono), y), ".top", font=mono, fill=ACCENT)
    for i, line in enumerate(["Monitoring for Algorand nodes.", "A terminal UI, a daemon that keeps", "history, and a web UI. One binary."]):
        d.text((x, 228 + i * 48), line, font=sans, fill=MUTED)
    d.text((x, H - 92), "go install algod.top@latest", font=small, fill=ACCENT)
    shot = Image.open(os.path.join(SITE, "img", "fleet-dark.webp")).convert("RGB")
    sw = 700
    shot = shot.resize((sw, round(shot.height * sw / shot.width)), Image.LANCZOS)
    sx, sy = W - sw + 60, 250
    mask = Image.new("L", shot.size, 0)
    ImageDraw.Draw(mask).rounded_rectangle((0, 0, shot.width - 1, shot.height - 1), radius=14, fill=255)
    shadow = Image.new("RGBA", (shot.width + 80, shot.height + 80), (0, 0, 0, 0))
    ImageDraw.Draw(shadow).rounded_rectangle((40, 48, shot.width + 40, shot.height + 48), radius=14, fill=(0, 0, 0, 150))
    shadow = shadow.filter(ImageFilter.GaussianBlur(18))
    img.paste(shadow, (sx - 40, sy - 40), shadow)
    img.paste(shot, (sx, sy), mask)
    ImageDraw.Draw(img).rounded_rectangle((sx, sy, sx + shot.width - 1, sy + shot.height - 1), radius=14, outline=LINE, width=2)
    img.save(os.path.join(SITE, "img", "social.png"), optimize=True)


if __name__ == "__main__":
    icons()
    social()
    print("wrote the favicons and site/img/social.png", file=sys.stderr)
