# UI demo

The stock term-llm frontend with a small, in-browser mock API that streams canned
answers. It is labelled as a scripted UI demo—not working AI inference.

No model download, Go/WASM runtime, shell, Python, API key, or application backend.
Chats, drafts and preferences exist only in the current tab and reset on reload.
The API mock never sends chat messages over the network. No service worker is
registered. Unsupported features are not simulated as real execution.

```sh
make frontend
node --test demo/mock-api.test.mjs
node demo/build.mjs
```

Serve `demo/dist/` with any static HTTP server. Production URL:
https://demo.term-llm.com/

No frontend source changes or new npm dependencies are needed. The build reuses
its existing asset manifest and HTML shell. To refresh the deployed UI, rebuild
from the current frontend rather than maintaining a second UI.
