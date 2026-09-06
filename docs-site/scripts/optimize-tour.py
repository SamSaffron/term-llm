"""Generate responsive WebP tour assets from capture-tour.mjs PNGs (requires Pillow)."""
from pathlib import Path
from PIL import Image

site = Path(__file__).resolve().parents[1]
source = site / "test-results" / "tour-source"
target = site / "static" / "images" / "tour"
target.mkdir(parents=True, exist_ok=True)
for scene in ("review", "hub", "worktrees", "agents", "shell"):
    for theme in ("light", "dark"):
        name = f"{scene}-{theme}"
        with Image.open(source / f"{name}.png") as image:
            # Full-size text remains pixel-identical; compact previews use high-quality WebP.
            image.save(target / f"{name}.webp", "WEBP", lossless=True, method=6)
            preview = image.resize((900, 478), Image.Resampling.LANCZOS)
            preview.save(target / f"{name}-900.webp", "WEBP", quality=90, method=6)
        print(name, "optimized")
