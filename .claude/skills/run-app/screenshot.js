// Screenshots of the running app (http://localhost:8080) in light/desktop and
// dark/phone, plus the settings dialog. Prints page errors per viewport.
// Logs in when the login page shows, as SRR_EMAIL / SRR_PASSWORD (defaults:
// the admin created in SKILL.md).
// Usage: NODE_PATH=$(npm root -g) node screenshot.js <out-dir> [url]
const { chromium } = require("playwright");

(async () => {
  const out = process.argv[2] || ".";
  const url = process.argv[3] || "http://localhost:8080/";
  const browser = await chromium.launch();
  const views = [
    ["desktop-light", { width: 1280, height: 800 }, "light"],
    ["phone-dark", { width: 390, height: 844 }, "dark"],
  ];
  for (const [name, viewport, colorScheme] of views) {
    const page = await browser.newPage({ viewport, colorScheme });
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    // A 401 before logging in is expected.
    page.on("console", (m) => m.type() === "error" && !m.text().includes("401") && errors.push(m.text()));
    await page.goto(url);
    await page.waitForTimeout(500);
    if (await page.isVisible("#loginView")) {
      await page.fill("#loginForm [name=email]", process.env.SRR_EMAIL || "admin@example.com");
      await page.fill("#loginForm [name=password]", process.env.SRR_PASSWORD || "test-admin-pw");
      await page.click("#loginForm [type=submit]");
    }
    await page.waitForTimeout(3500); // status poll + snapshot
    await page.screenshot({ path: `${out}/${name}.png`, fullPage: true });
    if (name === "desktop-light") {
      await page.click("#settingsBtn");
      await page.waitForTimeout(500);
      await page.screenshot({ path: `${out}/settings.png` });
    }
    console.log(name, errors.length ? errors : "no errors");
  }
  await browser.close();
})();
