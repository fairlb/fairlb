#!/usr/bin/env python3
"""Verify .env.example promises through Docker Compose's own interpolation."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[1]
PROMISE = re.compile(r"^#([A-Z][A-Z0-9_]*)=")


def run(*args: str, env: dict[str, str]) -> str:
    result = subprocess.run(
        args, cwd=ROOT, env=env, check=False, text=True, capture_output=True
    )
    if result.returncode:
        raise SystemExit(result.stderr or result.stdout)
    return result.stdout


def main() -> None:
    names = {
        match.group(1)
        for line in (ROOT / ".env.example").read_text().splitlines()
        if (match := PROMISE.match(line))
    }
    host_only = {"PORT"}
    app_names = names - host_only
    if len(app_names) < 15:
        raise SystemExit(f"compose-env: only found {len(app_names)} promised app variables")

    sentinels = {name: f"fairlb_compose_{name.lower()}" for name in names}
    sentinels["PUBLIC_URL"] = "https://compose.example"
    sentinels["PORT"] = "18080"
    env = os.environ.copy()
    for name in names:
        env.pop(name, None)

    with tempfile.TemporaryDirectory(prefix="fairlb-compose-") as directory:
        env_file = Path(directory) / "surface.env"
        env_file.write_text("".join(f"{name}={value}\n" for name, value in sorted(sentinels.items())))
        interpolation = run(
            "docker", "compose", "--env-file", str(env_file), "config", "--environment", env=env
        )
        interpolation_map = dict(
            line.split("=", 1) for line in interpolation.splitlines() if "=" in line
        )
        missing = [name for name in names if interpolation_map.get(name) != sentinels[name]]
        if missing:
            raise SystemExit(f"compose-env: Compose did not interpolate promised variables: {missing}")

        rendered = run(
            "docker", "compose", "--env-file", str(env_file), "config", "--format", "json", env=env
        )
        service_env = json.loads(rendered)["services"]["fairlb"]["environment"]
        missing = [name for name in app_names if service_env.get(name) != sentinels[name]]
        if missing:
            raise SystemExit(f"compose-env: .env.example values do not reach the container: {missing}")

        surface = {
            line.strip()
            for line in (ROOT / "internal/community/config/testdata/env-surface.txt").read_text().splitlines()
            if line.strip() and not line.startswith("#")
        }
        canonical = {name for name in surface if not name.endswith("_FILE")} - {"ENV"}
        if missing_delivery := canonical - service_env.keys():
            raise SystemExit(
                f"compose-env: Community runtime variables lack a Compose delivery path: {sorted(missing_delivery)}"
            )

        precedence_env = env | {"LOG_FORMAT": "process-wins"}
        precedence = run(
            "docker", "compose", "--env-file", str(env_file), "config", "--environment", env=precedence_env
        )
        if "LOG_FORMAT=process-wins" not in precedence.splitlines():
            raise SystemExit("compose-env: process environment did not override --env-file as expected")

    print(
        f"compose-env: ok ({len(app_names)} .env promises; {len(canonical)} runtime delivery paths; "
        "Compose precedence verified)"
    )


if __name__ == "__main__":
    main()
