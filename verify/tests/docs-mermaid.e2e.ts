import { readdirSync, readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { describe, test } from '@e2e-dev/web';
import { expect } from 'e2e';

// The docs are rendered from a host that no request reaches, so the page loads
// mermaid from node_modules and nothing from a CDN.
const origin = 'http://docs.verify.test';
const root = fileURLToPath(new URL('../../', import.meta.url));
const mermaidScript = createRequire(import.meta.url).resolve('mermaid/dist/mermaid.min.js');

type Block = { where: string; source: string };

function mermaidBlocks(): Block[] {
  const files = ['README.md', ...readdirSync(`${root}docs`).filter((name) => name.endsWith('.md')).map((name) => `docs/${name}`)];
  const blocks: Block[] = [];
  for (const file of files) {
    const lines = readFileSync(`${root}${file}`, 'utf8').split('\n');
    for (let i = 0; i < lines.length; i++) {
      if (lines[i].trim() !== '```mermaid') continue;
      const end = lines.findIndex((line, j) => j > i && line.trim() === '```');
      expect(end, `${file}:${i + 1} opens a mermaid block that never closes`).toBeGreaterThan(i);
      blocks.push({ where: `${file}:${i + 1}`, source: lines.slice(i + 1, end).join('\n') });
      i = end;
    }
  }
  return blocks;
}

describe('docs', () => {
  test('every mermaid block in README.md and docs/ renders', async ({ browser }) => {
    const blocks = mermaidBlocks();
    expect(blocks.length).toBeGreaterThan(0);

    await browser.route(`${origin}/**`, async (route) => {
      if (route.request.url === `${origin}/mermaid.min.js`) {
        await route.fulfill({ path: mermaidScript, contentType: 'text/javascript' });
        return;
      }
      await route.fulfill({ contentType: 'text/html', body: '<!doctype html><script src="/mermaid.min.js"></script><body></body>' });
    });
    await browser.goto(`${origin}/`);

    const failures = await browser.evaluate(async (arg) => {
      const all = arg as Block[];
      const mermaid = (globalThis as unknown as { mermaid: { initialize(c: object): void; render(id: string, text: string): Promise<unknown> } }).mermaid;
      mermaid.initialize({ startOnLoad: false });
      const out: string[] = [];
      for (const [i, block] of all.entries()) {
        try {
          await mermaid.render(`block${i}`, block.source);
        } catch (error) {
          out.push(`${block.where}: ${error instanceof Error ? error.message : String(error)}`);
        }
      }
      return out;
    }, blocks);

    expect(failures).toEqual([]);
  });
});
