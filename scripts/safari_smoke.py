#!/usr/bin/env python3
"""Bounded real Safari smoke; Python 3.9+ stdlib, no Selenium or OS changes."""
import argparse
import functools
import http.server
import json
import subprocess
import threading
import time
import urllib.parse
import urllib.request

ELEMENT = "element-6066-11e4-a52e-4f735466cecf"
PATCH = "--- a/smoke.txt\n+++ b/smoke.txt\n@@ -1 +1 @@\n-old\n+safari-smoke-diff\n"


class Fixture(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        url = urllib.parse.urlparse(self.path)
        if url.path == "/diff":
            time.sleep(6)
            body = json.dumps({"diff": PATCH}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            super().do_GET()

    def log_message(self, *_args):
        pass


class Driver:
    def __init__(self, url):
        self.url = url.rstrip("/")
        self.session = None

    def request(self, method, path, body=None):
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(
            self.url + path, data=data, method=method,
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=30) as response:
            value = json.load(response)["value"]
        if isinstance(value, dict) and "error" in value:
            raise RuntimeError(value)
        return value

    def call(self, method, path, body=None):
        return self.request(method, "/session/" + self.session + path, body)

    def js(self, script, *args):
        return self.call("POST", "/execute/sync", {"script": script, "args": args})

    def find(self, selector):
        return self.call("POST", "/element", {"using": "css selector", "value": selector})[ELEMENT]

    def hover(self, selector):
        element = self.find(selector)
        # Hover the row first: its buttons have no layout until revealed.
        self.call("POST", "/actions", {"actions": [{
            "type": "pointer", "id": "mouse", "parameters": {"pointerType": "mouse"},
            "actions": [{"type": "pointerMove", "duration": 0,
                         "origin": {ELEMENT: element}, "x": 0, "y": 0}],
        }]})

    def click(self, selector):
        self.call("POST", "/element/" + self.find(selector) + "/click", {})

    def keys(self, selector, text):
        self.call("POST", "/element/" + self.find(selector) + "/value", {"text": text})

    def wait(self, script, timeout=12):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            result = self.js(script)
            if result:
                return result
            time.sleep(0.05)
        raise AssertionError("Timed out: " + script)


def run(driver, url, verify_pasteboard):
    created = driver.request("POST", "/session", {
        "capabilities": {"alwaysMatch": {"browserName": "safari"}},
    })
    driver.session = created["sessionId"]
    result = {"capabilities": created.get("capabilities"), "checks": []}
    try:
        driver.call("POST", "/window/rect", {"width": 1100, "height": 900})
        driver.call("POST", "/url", {"url": url})
        driver.wait("return !!window.safariSmoke")
        result["userAgent"] = driver.js("return navigator.userAgent")
        driver.click("#direct")
        driver.wait("return window.safariSmoke.direct === 'ok'")
        if verify_pasteboard:
            assert subprocess.check_output(["pbpaste"], text=True) == "safari-smoke-sentinel"
        result["checks"].append("trusted direct clipboard positive control")

        driver.hover('.diff-file-row')
        driver.click('[aria-label="Copy diff for smoke.txt"]')
        driver.wait("return window.safariSmoke.requests === 1")
        driver.wait("return document.querySelector('[aria-label=\"Copy diff for smoke.txt\"]').classList.contains('copied')")
        assert not driver.js("return window.safariSmoke.errors"), driver.js("return window.safariSmoke.errors")
        if verify_pasteboard:
            assert subprocess.check_output(["pbpaste"], text=True) == PATCH
        result["checks"].append("trusted unloaded diff copy after 6s network delay")

        # Actual pointer actions at all four wrapper corners; no DOM .click().
        rect = driver.js("const r=document.querySelector('.toggle-switch').getBoundingClientRect(); return {x:r.x,y:r.y,width:r.width,height:r.height}")
        result["toggleRect"] = rect
        for x, y in [(1, 1), (rect["width"]-2, 1),
                     (1, rect["height"]-2), (rect["width"]-2, rect["height"]-2)]:
            before = driver.js("return document.querySelector('.toggle-switch-input').checked")
            driver.call("POST", "/actions", {"actions": [{
                "type": "pointer", "id": "mouse", "parameters": {"pointerType": "mouse"},
                "actions": [
                    {"type": "pointerMove", "duration": 0, "origin": "viewport",
                     "x": round(rect["x"]+x), "y": round(rect["y"]+y)},
                    {"type": "pointerDown", "button": 0},
                    {"type": "pointerUp", "button": 0},
                ],
            }]})
            assert driver.js("return document.querySelector('.toggle-switch-input').checked") != before
        result["checks"].append("four trusted toggle wrapper-corner clicks")

        prompt = 'textarea[aria-label="Message"]'
        driver.keys(prompt, "draft")
        # Synthetic Safari-shaped confirmation protects the production handler,
        # but is explicitly NOT a native OS IME test.
        dispatch = """
          const input=document.querySelector('textarea[aria-label="Message"]');
          input.dispatchEvent(new KeyboardEvent('keydown', {
            key: arguments[0], keyCode: 229, isComposing: false,
            bubbles: true, cancelable: true
          }));
        """
        driver.js(dispatch, "Enter")
        assert driver.js("return window.safariSmoke.sends === 0 && document.querySelector('textarea').value === 'draft'")
        driver.keys(prompt, "\ue007")  # Real Enter still sends and clears.
        driver.wait("return window.safariSmoke.sends === 1 && document.querySelector('textarea').value === ''")
        driver.keys(prompt, "/co")
        driver.wait("return !!document.querySelector('[role=listbox]')")
        selected = driver.js("return document.querySelector('textarea').getAttribute('aria-activedescendant')")
        for key in ["ArrowDown", "ArrowUp", "Enter", "Tab", "Escape"]:
            driver.js(dispatch, key)
            assert driver.js("return document.querySelector('textarea').value") == "/co"
            assert driver.js("return document.querySelector('textarea').getAttribute('aria-activedescendant')") == selected
            assert driver.js("return !!document.querySelector('[role=listbox]')")
        driver.keys(prompt, "\ue007")
        driver.wait("return document.querySelector('textarea').value !== '/co'")
        assert driver.js("return window.safariSmoke.sends") == 1
        result["checks"].append("synthetic keyCode229 guard + real Enter send/completion")
        result["audit"] = driver.js("return window.safariSmoke")
        result["clipboardBytesVerified"] = verify_pasteboard
        result["limits"] = ["No native OS IME, microphone/hardware, iOS or full-app coverage."]
        return result
    finally:
        driver.request("DELETE", "/session/" + driver.session)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--fixture-dir", help="Built tmp/safari-smoke directory; start owned loopback server")
    source.add_argument("--fixture-url", help="Existing fixture server URL (must serve delayed /diff)")
    parser.add_argument("--driver-url", default="http://127.0.0.1:18969")
    parser.add_argument("--verify-pasteboard", action="store_true", help="Compare exact bytes using macOS pbpaste (same desktop user)")
    args = parser.parse_args()
    server = None
    try:
        url = args.fixture_url
        if args.fixture_dir:
            handler = functools.partial(Fixture, directory=args.fixture_dir)
            server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
            threading.Thread(target=server.serve_forever, daemon=True).start()
            url = "http://127.0.0.1:%d/" % server.server_port
        print(json.dumps(run(Driver(args.driver_url), url, args.verify_pasteboard), indent=2))
    finally:
        if server:
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    main()
