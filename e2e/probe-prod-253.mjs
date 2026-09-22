import { chromium } from '@playwright/test';
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1920, height: 1050 } });
await page.setExtraHTTPHeaders({ 'X-Forwarded-Preferred-Username': 'tibrez' });
const errors = [];
page.on('pageerror', (e) => errors.push(String(e)));

// wall: per-PR tokens distinct?
await page.goto('http://127.0.0.1:18084/', { waitUntil: 'networkidle' });
const wall = await page.evaluate(() => {
  const cells = [...document.querySelectorAll('.wall-tokens, [data-testid="wall-card"] td:nth-child(5)')];
  const vals = cells.map((c) => c.textContent.trim()).slice(0, 8);
  return { sample: vals };
});
console.log('wall token cells:', JSON.stringify(wall));

// live run detail: live lane + wide shell + full model id
await page.goto('http://127.0.0.1:18084/runs/attempt-pr-review-rhesadox-58eb7446e516', { waitUntil: 'networkidle' });
const m = await page.evaluate(() => {
  const live = document.querySelector('.rg-timing-live title');
  const content = document.querySelector('.ds-page-content').getBoundingClientRect();
  const modelTitle = [...document.querySelectorAll('#run-graph-svg .rg-title')].map((t) => t.textContent);
  return {
    liveTitle: live?.textContent ?? null,
    contentW: Math.round(content.width),
    viewportW: window.innerWidth,
    overflow: document.documentElement.scrollWidth > window.innerWidth,
    cardTitles: modelTitle,
  };
});
console.log('rhesadox run:', JSON.stringify(m), 'jsErrors:', errors.length);
await page.screenshot({ path: '/tmp/prod-253-rhesadox.png' });
await browser.close();
