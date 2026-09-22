import { chromium } from '@playwright/test';

const browser = await chromium.launch();
const url = 'http://127.0.0.1:8083/runs/attempt-pr-review-demo-42a1';

for (const width of [1280, 900, 1100]) {
  const page = await browser.newPage({ viewport: { width, height: 950 } });
  await page.setExtraHTTPHeaders({ 'X-Harmostes-Dev-User': 'fixture-user' });
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  await page.goto(url, { waitUntil: 'networkidle' });
  await page.locator('#run-graph-frag [data-node="gate"]').first().click();
  await page.waitForTimeout(300);

  const m = await page.evaluate(() => {
    const r = (el) => { if (!el) return null; const b = el.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), r: Math.round(b.right), b: Math.round(b.bottom) }; };
    const panel = r(document.getElementById('rg-panel'));
    const timing = r(document.querySelector('.run-graph-stream .rg-timing'));
    const stream = r(document.getElementById('run-graph-frag'));
    // clip probe: any duration text extending past the timing svg's right edge?
    const ts = document.querySelector('.run-graph-stream .rg-timing');
    const clipped = [];
    ts.querySelectorAll('text.rg-timing-dur').forEach((t) => {
      const tb = t.getBoundingClientRect();
      if (tb.right > ts.getBoundingClientRect().right - 1) clipped.push(t.textContent);
    });
    return { panelBelowStream: panel.y >= stream.b - 2, panelRightOfStream: panel.x >= stream.r - 2, timingW: Math.round(timing.r - timing.x), clippedText: clipped };
  });
  console.log(`@${width}:`, JSON.stringify(m), 'jsErrors:', errors.length);
  await page.screenshot({ path: `/tmp/fix-${width}.png`, fullPage: true });
  await page.close();
}
await browser.close();
