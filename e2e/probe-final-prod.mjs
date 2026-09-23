import { chromium } from '@playwright/test';
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 900, height: 950 } });
await page.setExtraHTTPHeaders({ 'X-Forwarded-Preferred-Username': 'tibrez' });
const errors = [];
page.on('pageerror', (e) => errors.push(String(e)));
await page.goto('http://127.0.0.1:18084/runs/attempt-pr-review-rhesadox-58eb7446e516', { waitUntil: 'networkidle' });
await page.locator('#run-graph-frag [data-node]').first().click();
await page.waitForTimeout(400);
const m = await page.evaluate(() => {
  const r = (el) => { if (!el) return null; const b = el.getBoundingClientRect(); return { y: Math.round(b.y), b: Math.round(b.bottom), x: Math.round(b.x), r: Math.round(b.right) }; };
  const panel = r(document.getElementById('rg-panel'));
  const stream = r(document.getElementById('run-graph-frag'));
  const ts = document.querySelector('.run-graph-stream .rg-timing');
  const clipped = [];
  ts.querySelectorAll('text.rg-timing-dur').forEach((t) => {
    if (t.getBoundingClientRect().right > ts.getBoundingClientRect().right - 1) clipped.push(t.textContent);
  });
  return { panelBelow: panel.y >= stream.b - 2, timingRight: Math.round(ts.getBoundingClientRect().right), streamRight: stream.r, clipped, pageOverflow: document.documentElement.scrollWidth > window.innerWidth };
});
console.log(JSON.stringify(m), 'jsErrors:', errors.length);
await page.screenshot({ path: '/tmp/prod-final-900.png', fullPage: false });
await browser.close();
