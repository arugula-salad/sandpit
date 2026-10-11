#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "sprites-py==0.7.1",
#   "e2b==2.52.0",
#   "daytona==0.220.0",
#   "vercel-sandbox==0.7.0",
# ]
# ///
"""Try substrate.inevitable.fyi's APIs from any machine, with the official SDKs (docs/sandboxes.md).
They are served by sandpit (github.com/arugula-salad/sandpit) on Agent Substrate.

    curl -O https://substrate.inevitable.fyi/try.py
    SANDBOXD_API_KEY=... uv run try.py            # all four
    SANDBOXD_API_KEY=... uv run try.py e2b        # or sprites, e2b, vercel, daytona

(This copy is sandpit's k8s/site/try.py, which the site serves; home-cloud's
scripts/sandboxd-try.py is the same script with sandbox.inevitable.fyi as its default.)

Each one creates a sandbox, runs a command in it, serves a page on a port and
fetches it through the sandbox's public URL, then deletes the sandbox. Exit
status is the number of APIs that failed. Needs only uv and an API key (Jake
makes them with `sandpitd keys create`). SANDBOXD_DOMAIN overrides the parent
domain (default substrate.inevitable.fyi).
"""

import asyncio
import os
import sys
import time
import urllib.request

DOMAIN = os.environ.get("SANDBOXD_DOMAIN", "substrate.inevitable.fyi")
KEY = os.environ.get("SANDBOXD_API_KEY", "")
PAGE = "hello from sandboxd"


def fetch(url, headers=None, tries=20):
    """GET url, retrying while the server in the sandbox starts."""
    last = None
    for _ in range(tries):
        try:
            req = urllib.request.Request(url, headers=headers or {})
            with urllib.request.urlopen(req, timeout=10) as r:
                return r.read().decode().strip()
        except Exception as e:  # noqa: BLE001 - any failure is a retry
            last = e
            time.sleep(0.5)
    raise RuntimeError(f"GET {url}: {last}")


def try_sprites():
    import json

    from sprites import SpritesClient

    base = f"https://sprites.{DOMAIN}"
    client = SpritesClient(KEY, base_url=base)
    name = f"try-{int(time.time())}"
    sprite = client.create_sprite(name)
    try:
        print(f"  created {name}")
        out = sprite.command("sh", "-c", "uname -sr && python3 --version").output().decode()
        print("  ran:", " | ".join(out.split("\n")).strip(" |"))
        # A sprite's URL asks for a key until its url_settings.auth is "public".
        req = urllib.request.Request(f"{base}/v1/sprites/{name}", method="PUT",
                                     data=json.dumps({"url_settings": {"auth": "public"}}).encode(),
                                     headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=30) as r:
            url = json.load(r)["url"] + "/"
        sprite.command("sh", "-c", f"mkdir -p /tmp/www && echo '{PAGE}' > /tmp/www/index.html && "
                       "cd /tmp/www && nohup python3 -m http.server 8080 >/dev/null 2>&1 &").run()
        body = fetch(url, tries=40)
        assert body == PAGE, body
        print(f"  served: {url} -> {body!r}")
    finally:
        client.delete_sprite(name)
        print("  deleted")


def try_e2b():
    os.environ.setdefault("E2B_API_KEY", KEY)
    os.environ.setdefault("E2B_DOMAIN", f"e2b.{DOMAIN}")
    os.environ.setdefault("E2B_API_URL", f"https://e2b.{DOMAIN}")
    from e2b import Sandbox

    sbx = Sandbox.create(timeout=300)
    try:
        print(f"  created {sbx.sandbox_id}")
        r = sbx.commands.run("uname -sr && python3 --version")
        print("  ran:", " | ".join(r.stdout.split("\n")).strip(" |"))
        sbx.files.write("/home/user/www/index.html", PAGE)
        sbx.commands.run("cd /home/user/www && python3 -m http.server 8080", background=True)
        url = f"https://{sbx.get_host(8080)}/"
        body = fetch(url)
        assert body == PAGE, body
        print(f"  served: {url} -> {body!r}")
    finally:
        sbx.kill()
        print("  deleted")


def try_daytona():
    os.environ.setdefault("DAYTONA_API_KEY", KEY)
    os.environ.setdefault("DAYTONA_API_URL", f"https://daytona.{DOMAIN}/api")
    from daytona import Daytona

    daytona = Daytona()
    sbx = daytona.create(timeout=120)
    try:
        print(f"  created {sbx.id}")
        r = sbx.process.exec("uname -sr && python3 --version")
        print("  ran:", " | ".join(r.result.split("\n")).strip(" |"))
        sbx.process.exec(f"mkdir -p /tmp/www && echo '{PAGE}' > /tmp/www/index.html")
        sbx.process.exec("cd /tmp/www && nohup python3 -m http.server 8080 >/dev/null 2>&1 &")
        link = sbx.get_preview_link(8080)
        body = fetch(link.url + "/", {"x-daytona-preview-token": link.token})
        assert body == PAGE, body
        print(f"  served: {link.url}/ -> {body!r}")
    finally:
        sbx.delete()
        print("  deleted")


def try_vercel():
    # The SDK wants these set even though sandpit only checks the token.
    os.environ.setdefault("VERCEL_TOKEN", KEY)
    os.environ.setdefault("VERCEL_TEAM_ID", "team_sandboxd")
    os.environ.setdefault("VERCEL_PROJECT_ID", "prj_sandboxd")
    from vercel import sandbox
    from vercel.api import session
    from vercel.sandbox import SandboxServiceOptions

    async def go():
        async with session(service_options=[SandboxServiceOptions(base_url=f"https://vercel.{DOMAIN}/api")]):
            box = await sandbox.create_sandbox(persistent=False, ports=[3000], execution_time_limit=300,
                                               resources=sandbox.SandboxResources(vcpus=1))
            try:
                print(f"  created {box.name}")
                r = await box.run_process("sh", ["-c", "uname -sr && python3 --version"], capture_output=True)
                print("  ran:", " | ".join(r.stdout.split("\n")).strip(" |"))
                await box.run_process("sh", ["-c", f"mkdir -p /tmp/www && echo '{PAGE}' > /tmp/www/index.html"])
                await box.create_process("python3", ["-m", "http.server", "3000", "--directory", "/tmp/www"])
                url = next(r.url for r in box.routes if r.port == 3000) + "/"
                body = await asyncio.to_thread(fetch, url)
                assert body == PAGE, body
                print(f"  served: {url} -> {body!r}")
            finally:
                await box.stop()
                await box.destroy(delete_orphan_snapshots=True)
                print("  deleted")

    asyncio.run(go())


APIS = {"sprites": try_sprites, "e2b": try_e2b, "vercel": try_vercel, "daytona": try_daytona}


def main():
    if not KEY:
        sys.exit("set SANDBOXD_API_KEY (on kubectl -n sandpit exec deploy/sandpitd -- /sandpitd keys create <name> --data /data)")
    names = sys.argv[1:] or list(APIS)
    unknown = [n for n in names if n not in APIS]
    if unknown:
        sys.exit(f"unknown API {unknown}: pick from {list(APIS)}")
    failed = 0
    for name in names:
        print(f"{name} (https://{name}.{DOMAIN})")
        t = time.time()
        try:
            APIS[name]()
            print(f"  ok in {time.time() - t:.1f}s")
        except Exception as e:  # noqa: BLE001 - report and go on to the next API
            failed += 1
            print(f"  FAILED: {type(e).__name__}: {e}")
    sys.exit(failed)


if __name__ == "__main__":
    main()
