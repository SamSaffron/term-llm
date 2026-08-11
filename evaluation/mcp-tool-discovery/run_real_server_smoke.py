#!/usr/bin/env python3
"""Best-effort real MCP schema smoke for the ten implementation categories.

The caller supplies an explicit, disposable mcp.json with exact package/image
versions and a metadata JSON documenting substitutions. Credentials are never
copied into results. The deterministic federation remains the 200-tool baseline.
"""
from __future__ import annotations
import argparse, datetime as dt, json, pathlib, subprocess

REQUIRED = {"github", "playwright", "chrome_devtools", "filesystem", "git", "postgresql", "sqlite", "fetch", "memory", "time"}
ROOT = pathlib.Path(__file__).resolve().parents[2]
SENSITIVE_KEY_PARTS = {"authorization", "credential", "header", "password", "secret", "token", "api_key"}


def contains_sensitive_metadata(value: object) -> bool:
    if isinstance(value, dict):
        for key, child in value.items():
            normalized = str(key).lower().replace("-", "_")
            if any(part in normalized for part in SENSITIVE_KEY_PARTS):
                return True
            if contains_sensitive_metadata(child):
                return True
    elif isinstance(value, list):
        return any(contains_sensitive_metadata(child) for child in value)
    return False


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--mcp-config", required=True, type=pathlib.Path)
    p.add_argument("--metadata", required=True, type=pathlib.Path, help="versions, roots, substitutions, and safety notes")
    p.add_argument("--scratch", type=pathlib.Path, default=pathlib.Path.home()/"scratch"/f"{dt.date.today().isoformat()}-mcp-tool-discovery-eval")
    a = p.parse_args()
    scratch = a.scratch.expanduser().resolve()
    if scratch.name != f"{dt.date.today().isoformat()}-mcp-tool-discovery-eval":
        p.error("--scratch must use the dated mcp-tool-discovery-eval directory")
    cfg = json.loads(a.mcp_config.read_text())
    names = set(cfg.get("servers", {}))
    if len(names) != 10:
        p.error(f"real smoke config must contain exactly 10 servers, got {len(names)}")
    metadata = json.loads(a.metadata.read_text())
    if contains_sensitive_metadata(metadata):
        p.error("metadata contains a credential/header/token-like field; record only versions, roots, substitutions, and safety notes")
    if not metadata.get("servers") or set(metadata["servers"]) != names:
        p.error("metadata.servers must document every configured server")
    # Category substitutions are explicit; exact names may differ when recorded.
    missing = REQUIRED - set(metadata.get("categories", []))
    if missing:
        p.error(f"metadata.categories missing: {sorted(missing)}")
    out_dir = scratch/"results"
    out_dir.mkdir(parents=True, exist_ok=True)
    binary = scratch/"bin/mcp-discovery-eval"
    binary.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(["go", "build", "-o", str(binary), "./internal/tooldiscovery/evalcmd"], cwd=ROOT, check=True)
    manifest = out_dir/"real-server-catalogue.json"
    subprocess.run([str(binary), "catalogue", "--mcp-config", str(a.mcp_config.resolve()), "--output", str(manifest), "--timeout", "2m"], cwd=ROOT, check=True)
    catalogue = json.loads(manifest.read_text())
    counts: dict[str, int] = {}
    for tool in catalogue["tools"]:
        counts[tool["server"]] = counts.get(tool["server"], 0) + 1
    report = {"measured_at": dt.datetime.now(dt.timezone.utc).isoformat(), "actual_total_tools": len(catalogue["tools"]),
              "actual_counts": counts, "catalogue_hash": catalogue["hash"], "metadata": metadata,
              "note": "Best-effort real-server smoke; the deterministic federation is the authoritative 200-tool comparison."}
    (out_dir/"real-server-smoke.json").write_text(json.dumps(report, indent=2, sort_keys=True)+"\n")
    print(out_dir/"real-server-smoke.json")
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
