import { chromium } from '@playwright/test';

const browser = await chromium.launch();
const url = 'http://127.0.0.1:8083/runs/attempt-pr-review-demo-42a1';
const auth = { 'X-Harmostes-Dev-User': 'fixture-user' };

for (const width of [1440, 1180, 900]) {
  const page = await browser.newPage({ viewport: { width, height: 900 } });
  await page.setExtraHTTPHeaders(auth);
  await page.goto(url);
  await page.waitForLoadState('networkidle');
  // click the gate node to fill the panel (long summary — the #551 shape)
  await page.locator('#run-graph-frag [data-node="gate"]').first().click();
  await page.waitForTimeout(400);

  // geometric overlap probe: panel rect vs graph-svg rect vs timing svg rect
  const m = await page.evaluate(() => {
    const r = (el) => { if (!el) return null; const b = el.getBoundingClientRect(); return { x: b.x, y: b.y, w: b.width, h: b.height, right: b.right, bottom: b.bottom }; };
    const svg = document.getElementById('run-graph-svg');
    const timing = document.querySelector('.rg-timing');
    const panel = document.getElementById('rg-panel');
    const layout = document.querySelector('.run-graph-layout');
    const overlap = (a, b) => a && b && !(a.right <= b.x + 1 || b.right <= a.x + 1 || a.bottom <= b.y + 1 || b.bottom <= a.y + 1);
    const rs = r(svg), rt = r(timing), rp = r(panel);
    // also elementFromPoint test at panel's top-left: what's actually on top?
    const cover = panel ? document.elementFromPoint(rp.x + rp.w / 2, rp.y + 10) : null;
    return {
      svg: rs, timing: rt, panel: rp,
      panelOverSvg: overlap(rp, rs),
      panelOverTiming: overlap(rp, rt),
      layoutW: r(layout)?.w,
      topElementAtPanel: cover ? cover.tagName + '.' + (cover.className.baseVal ?? cover.className) : null,
      svgScrollW: document.getElementById('run-graph-frag')?.scrollWidth,
      svgClientW: document.getElementById('run-graph-frag')?.clientWidth,
    };
  });
  console.log(`\n=== width ${width} ===`);
  console.log(JSON.stringify(m, null, 1));
  await page.screenshot({ path: `/tmp/overlap-${width}.png`, fullPage: false });
  await page.close();
}
await browser.close();
