# UI demo

The stock term-llm frontend with a small, in-browser mock API that streams canned
answers. It is labelled as a scripted UI demo—not working AI inference.

No model download, Go/WASM runtime, shell, Python, API key, or application backend.
Chats, drafts and preferences exist only in the current tab and reset on reload.
The API mock never sends chat messages or device-tool results to an application
backend. No service worker is registered. Unsupported features are not simulated
as real execution.

## Native iOS walkthrough

In the term-llm iOS app, choose **Try demo** on first launch, or connect to
`https://demo.term-llm.com/`. Ordinary browsers can explore the chat UI without
native tools. The scripted welcome/help explains the native integration:

1. Pull down near the top of the page for app controls, then open **Settings**.
2. Under **Device tools**, turn on **Share device tools with this server** and
   tap **Done**. This is explicit per-server permission, not automatic demo setup.
3. In chat, open **+ → MCP servers** and enable the **iPhone/iPad** entry labelled
   **WebMCP from this page**. Close the dialog.
4. Send **create playlist** to call `playlist_create_demo`. The app creates a
   real, locally saved playlist with five fictional sample songs. No Apple Music
   authorization, subscription or playback is needed. Repeating this command
   returns the same playlist without resetting user edits.
5. Pull down for app controls and tap **♪ Music**. The sample playlist can be
   inspected, renamed, reordered and deleted in the native UI. Its fictional
   tracks cannot be played; real songs can be added through Apple Music search.

Also try **list playlists**, **ping**, or **device info** after enabling their
native tools. **Help** repeats the walkthrough. No native tools run on startup,
and the demo never automatically enables sharing or conversation tools. It does
not request location, microphone or Apple Music access, and it does not pretend
to provide live voice, navigation or music playback.

**Native changes persist even when demo chats reset.** Delete the sample playlist
from Music when finished. Updating only this website is insufficient for the
sample-playlist action: the iOS build must expose `playlist_create_demo`.

### Tool protocol

Only explicit supported commands can request tools, and only exact tool names
advertised in the current response request's `tools` list are used. Tool calls
use the stock frontend's `response.client_tool.requested` event and inline
`client_tool_calls/{call_id}/result` endpoint, so execution goes through the real
WebMCP client and native origin checks—not a demo-specific native shortcut.
Confirmations are based on native results; unavailable tools, errors, timeouts
and cancellation do not produce fabricated success. Stopping a response cannot
undo a native action that has already reached the device.

## Build and test

```sh
make frontend
node --test demo/mock-api.test.mjs demo/hub.test.mjs
node demo/build.mjs
```

For root-only chat, serve `demo/dist/` with any static HTTP server, for example:

```sh
python3 -m http.server 8765 --directory demo/dist
```

The sibling iOS repository has an opt-in simulator test that drives the real
frontend MCP switch and creates a sample playlist through the actual injected
bridge, with Apple Music denied and an isolated native playlist store. See its
README for `TEST_RUNNER_TERMLLM_DEMO_E2E_URL`.

Production URL: https://demo.term-llm.com/

No frontend source changes or new npm dependencies are needed. The build reuses
its existing asset manifest and HTML shell. To refresh the deployed UI, rebuild
from the current frontend rather than maintaining a second UI. Publish the
rebuilt `demo/dist/` through the normal static-site deployment; local edits/builds
do not update the production URL automatically.

## Static native Hub connection

The same origin also publishes a read-only Hub inventory: one clearly labelled
DEMO node and two fixed scripted conversations. The root remains the ordinary
interactive demo (including the native device-tool walkthrough), not a second
Hub dashboard. Native clients can discover the Hub and render their own UI.

**Hosting must explicitly route the extensionless JSON endpoints and nested chat
paths.** See [ROUTING.md](ROUTING.md) for the endpoint manifest, exact proposed
nginx snippet, local static-server/browser tests, read-only protocol behavior,
and iOS acceptance limitations. Merely copying build assets does not change an
existing server's routing. No production configuration or deployment is performed
by this change.
