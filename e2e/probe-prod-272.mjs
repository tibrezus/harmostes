import { chromium } from '@playwright/test';
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1920, height: 1050 } });
await page.setExtraHTTPHeaders({ 'X-Forwarded-Preferred-Username': 'tibrez' });
const errors = [];
page.on('pageerror', (e) => errors.push(String(e)));
await page.goto('http://127.0.0.1:18084/', { waitUntil: 'networkidle' });
await page.waitForTimeout(1500);
const m = await page.evaluate(() => ({
  version: document.body.innerText.match(/1\.2\.0-\d+/)?.[0],
  counts: document.querySelector('[data-testid="wall-counts"]')?.textContent?.trim().replace(/\s+/g, ' '),
  liveTokens: [...document.querySelectorAll('[data-testid="wall-live-tokens"]')].map((c) => ({
    subject: c.closest('[data-testid="wall-card"]')?.getAttribute('data-subject'),
    text: c.textContent.trim().replace(/\s+/g, ' '),
    title: c.getAttribute('title'),
    model: c.parentElement?.querySelector('.wall-usage-model')?.textContent,
  })),
}));
console.log(JSON.stringify(m, null, 1));
console.log('jsErrors:', errors.length);
await browser.close();
