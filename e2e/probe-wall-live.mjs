import { chromium } from '@playwright/test';
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1920, height: 1050 } });
await page.setExtraHTTPHeaders({ 'X-Harmostes-Dev-User': 'fixture-user' });
await page.goto('http://127.0.0.1:8083/', { waitUntil: 'networkidle' });
const m = await page.evaluate(() => ({
  cards: document.querySelectorAll('[data-testid="wall-card"]').length,
  currents: [...document.querySelectorAll('[data-testid="wall-current"]')].map((c) => c.textContent.trim().replace(/\s+/g, ' ')),
  tokens: [...document.querySelectorAll('[data-testid="wall-card"] td:nth-child(6)')].map((c) => c.textContent.trim()).slice(0, 4),
  subjects: [...document.querySelectorAll('[data-testid="wall-card"]')].map((c) => c.getAttribute('data-subject')),
}));
console.log(JSON.stringify(m, null, 1));
await page.screenshot({ path: '/tmp/wall-live-final.png' });
await browser.close();
