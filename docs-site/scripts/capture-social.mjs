// Regenerate the share image without remote fonts or network assets.
import { chromium } from "playwright";
import { fileURLToPath } from "node:url";
const browser = await chromium.launch();
try {
  const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 });
  await page.setContent(`<!doctype html><html lang="en"><meta charset="utf-8"><title>term-llm</title><style>
    *{box-sizing:border-box}body{margin:0;background:#fcfcfa;color:#222b28;font-family:system-ui,sans-serif;padding:64px 76px;width:1200px;height:630px}
    header{display:flex;align-items:center;gap:14px;font-size:30px;font-weight:700;letter-spacing:-1px}.mark{display:grid;place-items:center;background:#16634e;color:white;border-radius:12px;width:52px;height:52px;font:700 28px monospace}
    h1{font-size:65px;line-height:1.12;letter-spacing:-3px;font-weight:650;margin:63px 0 24px}h1 span{color:#16634e}p{font-size:23px;color:#5b6660;margin:0;line-height:1.6}footer{border-top:1px solid #dce1da;margin-top:46px;padding-top:23px;display:flex;justify-content:space-between;font-size:18px;color:#5b6660}.url{color:#16634e;font-weight:600}
  </style><header><span class="mark">&gt;_</span>term-llm</header><h1>AI in your terminal.<br><span>A workspace in your browser.</span></h1><p>Your models. Your tools. Your workspace.<br>Code, research, and automate with a self-hosted AI runtime.</p><footer><span>Open source · Terminal-first · Browser-ready</span><span class="url">term-llm.com</span></footer></html>`);
  await page.screenshot({ path: fileURLToPath(new URL("../static/images/social-card.png", import.meta.url)) });
} finally { await browser.close(); }
