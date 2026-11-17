// Renders each [data-picture] element of docs/diagrams/source.html to
// docs/images/<name>-light.png and <name>-dark.png, then records the hash of
// the source in docs/images/SOURCE_SHA256, in sha256sum format. The hash is
// written last, so a run that fails leaves the old hash and the images read
// as stale.
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, join, relative } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { chromium } from "playwright-core";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const sourceName = "docs/diagrams/source.html";
const source = join(root, ...sourceName.split("/"));
const images = join(root, "docs", "images");
const schemes = ["light", "dark"];

function checkNames(names) {
  if (names.length === 0) {
    throw new Error(`${sourceName} has no [data-picture] element`);
  }
  const seen = new Set();
  for (const name of names) {
    if (!/^[a-z][a-z0-9-]*$/.test(name)) {
      throw new Error(`picture name ${JSON.stringify(name)} must match [a-z][a-z0-9-]*`);
    }
    if (seen.has(name)) {
      throw new Error(`picture name ${JSON.stringify(name)} repeats`);
    }
    seen.add(name);
  }
}

async function render() {
  const html = await readFile(source);
  const browser = await chromium.launch();
  try {
    const page = await browser.newPage({
      viewport: { width: 1200, height: 900 },
      deviceScaleFactor: 2,
    });
    await page.goto(pathToFileURL(source).href);
    const pictures = page.locator("[data-picture]");
    const names = await pictures.evaluateAll((els) => els.map((el) => el.dataset.picture));
    checkNames(names);
    await mkdir(images, { recursive: true });
    for (const [i, name] of names.entries()) {
      for (const colorScheme of schemes) {
        await page.emulateMedia({ colorScheme });
        const file = join(images, `${name}-${colorScheme}.png`);
        await pictures.nth(i).screenshot({ path: file, animations: "disabled" });
        console.log(relative(root, file));
      }
    }
  } finally {
    await browser.close();
  }
  const sum = join(images, "SOURCE_SHA256");
  const hex = createHash("sha256").update(html).digest("hex");
  await writeFile(sum, `${hex}  ${sourceName}\n`);
  console.log(relative(root, sum));
}

try {
  await render();
} catch (err) {
  console.error(`docs-images: ${err.message}`);
  process.exit(1);
}
