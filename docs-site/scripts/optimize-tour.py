"""Generate responsive WebP tour assets (requires Pillow).

By default, optimize the browser scenes captured by capture-tour.mjs. Use
--terminal-source PATH to publish an approved standalone terminal PNG instead.
"""
import argparse
from pathlib import Path
from PIL import Image

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--terminal-source", type=Path)
args = parser.parse_args()

site = Path(__file__).resolve().parents[1]
source = site / "test-results" / "tour-source"
target = site / "static" / "images" / "tour"
target.mkdir(parents=True, exist_ok=True)
if args.terminal_source:
    images = [("terminal-dark", args.terminal_source)]
else:
    images = [
        (f"{scene}-{theme}", source / f"{scene}-{theme}.png")
        for scene in ("review", "hub", "worktrees", "agents", "shell")
        for theme in ("light", "dark")
    ]
for name, path in images:
    with Image.open(path) as image:
        if image.size != (2560, 1360):
            raise ValueError(f"{path}: expected 2560×1360, got {image.size}")
        # Full-size text remains pixel-identical; compact previews use high-quality WebP.
        image.save(target / f"{name}.webp", "WEBP", lossless=True, method=6)
        preview = image.resize((900, 478), Image.Resampling.LANCZOS)
        preview.save(target / f"{name}-900.webp", "WEBP", quality=90, method=6)
    print(name, "optimized")
