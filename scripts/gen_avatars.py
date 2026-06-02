"""
Generate a simple stack-chan-style avatar PNG set (14 frames) for the
stackchan-mcp firmware's avatar build.

Outputs full-frame 320x240 RGB PNGs into ~/.stackchan/avatar/:
  faces : idle happy thinking sad surprised embarrassed
  eyes  : eyes_open eyes_half eyes_closed
  mouths: mouth_closed mouth_half mouth_open mouth_e mouth_u

Each frame is a complete face (white eyes + mouth on a dark background) so the
firmware can swap whole frames for blink / lip-sync. Minimal on purpose — a
placeholder you can later replace with real art (same filenames).

Run:  uv run --with pillow python scripts/gen_avatars.py
Then: cd <firmware> && python scripts/avatar_convert/convert_avatars.py
"""
import os

from PIL import Image, ImageDraw

W, H = 320, 240
BG = (18, 26, 38)        # dark navy
FG = (245, 247, 250)     # near-white features
BLUSH = (255, 120, 140)
TEAR = (120, 200, 255)

LEYE = (112, 102)        # left eye center
REYE = (208, 102)        # right eye center
MOUTH = (160, 170)       # mouth center

OUT = os.path.join(os.path.expanduser("~"), ".stackchan", "avatar")


def bbox(cx, cy, hw, hh):
    return [cx - hw, cy - hh, cx + hw, cy + hh]


def eye(d, center, style):
    cx, cy = center
    if style == "open":
        d.ellipse(bbox(cx, cy, 24, 26), fill=FG)
    elif style == "big":
        d.ellipse(bbox(cx, cy, 30, 32), fill=FG)
        d.ellipse(bbox(cx, cy + 2, 9, 9), fill=BG)  # small pupil hole -> surprised
    elif style == "half":
        d.pieslice(bbox(cx, cy, 24, 26), 20, 160, fill=FG)  # lower lid showing
    elif style == "closed":
        d.line([cx - 24, cy, cx + 24, cy], fill=FG, width=7)  # neutral closed
    elif style == "happy":
        d.arc(bbox(cx, cy, 26, 22), 200, 340, fill=FG, width=8)  # ^ caret
    elif style == "sad":
        d.arc(bbox(cx, cy, 26, 22), 20, 160, fill=FG, width=8)   # droopy valley
    elif style == "up":
        d.ellipse(bbox(cx, cy - 12, 22, 24), fill=FG)            # looking up


def mouth(d, style):
    cx, cy = MOUTH
    if style == "neutral":
        d.line([cx - 26, cy, cx + 26, cy], fill=FG, width=7)
    elif style == "smile":
        d.arc(bbox(cx, cy - 8, 40, 30), 20, 160, fill=FG, width=9)
    elif style == "frown":
        d.arc(bbox(cx, cy + 8, 40, 30), 200, 340, fill=FG, width=9)
    elif style == "wavy":  # embarrassed
        pts = []
        for i in range(0, 61):
            x = cx - 30 + i
            import math
            y = cy + int(6 * math.sin(i / 60 * 3.14159 * 3))
            pts.append((x, y))
        d.line(pts, fill=FG, width=6, joint="curve")
    elif style == "o":      # open round
        d.ellipse(bbox(cx, cy, 22, 26), fill=FG)
    elif style == "closed":
        d.line([cx - 24, cy, cx + 24, cy], fill=FG, width=7)
    elif style == "half":
        d.ellipse(bbox(cx, cy, 18, 12), fill=FG)
    elif style == "open":
        d.ellipse(bbox(cx, cy, 26, 30), fill=FG)
    elif style == "e":      # wide thin
        d.ellipse(bbox(cx, cy, 34, 10), fill=FG)
    elif style == "u":      # small round
        d.ellipse(bbox(cx, cy, 16, 18), fill=FG)


def blush(d):
    d.ellipse(bbox(78, 138, 18, 11), fill=BLUSH)
    d.ellipse(bbox(242, 138, 18, 11), fill=BLUSH)


def tear(d):
    d.polygon([(REYE[0] + 20, REYE[1] + 18), (REYE[0] + 12, REYE[1] + 40),
               (REYE[0] + 28, REYE[1] + 40)], fill=TEAR)


def dots(d):  # thinking "..."
    for i in range(3):
        d.ellipse(bbox(250 + i * 16, 56, 4, 4), fill=FG)


def frame(le, re, mo, extras=()):
    im = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(im)
    eye(d, LEYE, le)
    eye(d, REYE, re)
    mouth(d, mo)
    for ex in extras:
        ex(d)
    return im


FRAMES = {
    # Phase 1 faces
    "idle":        ("open", "open", "neutral", ()),
    "happy":       ("happy", "happy", "smile", ()),
    "thinking":    ("up", "up", "neutral", (dots,)),
    "sad":         ("sad", "sad", "frown", (tear,)),
    "surprised":   ("big", "big", "o", ()),
    "embarrassed": ("open", "open", "wavy", (blush,)),
    # Phase 2 eyes (neutral mouth)
    "eyes_open":   ("open", "open", "neutral", ()),
    "eyes_half":   ("half", "half", "neutral", ()),
    "eyes_closed": ("closed", "closed", "neutral", ()),
    # Phase 2 mouths (open eyes)
    "mouth_closed": ("open", "open", "closed", ()),
    "mouth_half":   ("open", "open", "half", ()),
    "mouth_open":   ("open", "open", "open", ()),
    "mouth_e":      ("open", "open", "e", ()),
    "mouth_u":      ("open", "open", "u", ()),
}


def main():
    os.makedirs(OUT, exist_ok=True)
    for name, (le, re, mo, extras) in FRAMES.items():
        im = frame(le, re, mo, extras)
        path = os.path.join(OUT, name + ".png")
        im.save(path)
        print("wrote", path)
    print(f"\n{len(FRAMES)} frames -> {OUT}")


if __name__ == "__main__":
    main()
