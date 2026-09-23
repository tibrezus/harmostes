import { chromium } from '@playwright/test';
const browser = await chromium.launch();

// 1. live attempt: growing bar + full-width shell at 1920
const page = await browser.newPage({ viewport: { width: 1920, height: 1050 } });
await page.setExtraHTTPHeaders({ 'X-Harmostes-Dev-User': 'fixture-user' });
await page.goto('http://127.0.0.1:8083/runs/attempt-pr-review-demo-43c2', { waitUntil: 'networkidle' });
const m1 = await page.evaluate(() => ({
  contentW: document.querySelector('.ds-page-content').getBoundingClientRect().width,
  viewportW: window.innerWidth,
  liveBars: document.querySelectorAll('.rg-timing-live').length,
  liveTitle: document.querySelector('.rg-timing-live title')?.textContent,
  pageOverflow: document.documentElement.scrollWidth > window.innerWidth,
}));
console.log('live @1920:', JSON.stringify(m1));
await page.screenshot({ path: '/tmp/final-live-1920.png' });
await page.close();

// 2. terminal attempt wide: layout sanity at 1920
const p2 = await browser.newPage({ viewport: { width: 1920, height: 1050 } });
await p2.setExtraHTTPHeaders({ 'X-Harmostes-Dev-User': 'fixture-user' });
await p2.goto('http://127.0.0.1:8083/runs/attempt-pr-review-demo-42a1', { waitUntil: 'networkidle' });
const m2 = await p2.evaluate(() => ({
  contentW: document.querySelector('.ds-page-content').getBoundingClientRect().width,
  liveBars: document.querySelectorAll('.rg-timing-live').length,
}));
console.log('terminal @1920:', JSON.stringify(m2));
await p2.screenshot({ path: '/tmp/final-terminal-1920.png' });
await p2.close();
await browser.close();
