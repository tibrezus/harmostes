import { chromium } from '@playwright/test';

const browser = await chromium.launch();
const targets = [
  '/runs/attempt-fork-maintenance-forgejo-weekly-fd5070d41991',
  '/runs/attempt-pr-review-rhesadox-58eb7446e516',
];
const widths = [1512, 1280, 1024];

for (const t of targets) {
  for (const width of widths) {
    const page = await browser.newPage({ viewport: { width, height: 950 } });
    await page.setExtraHTTPHeaders({ 'X-Forwarded-Preferred-Username': 'tibrez' });
    await page.goto('http://127.0.0.1:18084' + t, { waitUntil: 'networkidle' });
    // click the first node card in the graph to populate the panel
    const node = page.locator('#run-graph-frag [data-node]').first();
    const nodeCount = await page.locator('#run-graph-frag [data-node]').count();
    if (nodeCount > 0) { await node.click(); await page.waitForTimeout(300); }
    const m = await page.evaluate(() => {
      const r = (el) => { if (!el) return null; const b = el.getBoundingClientRect(); return { x: Math.round(b.x), y: Math.round(b.y), r: Math.round(b.right), b: Math.round(b.bottom) }; };
      const panel = document.getElementById('rg-panel');
      const stream = document.getElementById('run-graph-frag');
      // visual check: does anything painted by the stream extend under the panel?
      // sample points across the panel's face and ask who is on top
      const rp = r(panel);
      const intruders = new Set();
      if (rp) for (let dx = 0.25; dx <= 0.75; dx += 0.25) for (const dy of [0.1, 0.3, 0.5, 0.7, 0.9]) {
        const el = document.elementFromPoint(rp.x + (rp.r - rp.x) * dx, rp.y + (rp.b - rp.y) * dy);
        if (el && !panel.contains(el) && el !== panel) intruders.add(el.tagName + '|' + (el.className.baseVal ?? el.className ?? '') + '|' + (el.id ?? ''));
      }
      return {
        panel: rp, stream: r(stream),
        svgW: document.getElementById('run-graph-svg')?.getAttribute('width'),
        streamScroll: stream ? [stream.scrollWidth, stream.clientWidth] : null,
        intruders: [...intruders],
      };
    });
    console.log(`${t.split('/runs/')[1].slice(0, 40)} @${width}: nodes=${nodeCount} svgW=${m.svgW} scroll=${m.streamScroll} intruders=${JSON.stringify(m.intruders)}`);
    await page.screenshot({ path: `/tmp/prod-${width}-${targets.indexOf(t)}.png` });
    await page.close();
  }
}
await browser.close();
