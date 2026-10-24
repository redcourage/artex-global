import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const presets = path.join(web, "src/styles/presets");
const allPalettes = JSON.parse(fs.readFileSync(path.join(presets, "security-palettes.json"), "utf8"));
const requestedPreset = process.argv[3];
if (requestedPreset && !allPalettes.some(p => p.id === requestedPreset)) {
  throw new Error(`Unknown preset: ${requestedPreset}`);
}
const palettes = requestedPreset ? allPalettes.filter(p => p.id === requestedPreset) : allPalettes;
const config = {
  initialPreset: requestedPreset ?? "contrast-black",
  initialMode: requestedPreset ? palettes[0].mode : "light",
  presentation: Boolean(requestedPreset),
};
const styles = palettes.map(p => fs.readFileSync(path.join(presets, `${p.id}.css`), "utf8")).join("\n");
const model = ts.transpileModule(fs.readFileSync(path.join(web, "src/lib/panel-dock.ts"), "utf8"), {
  compilerOptions: { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.ES2020 },
}).outputText.replace(/^export /gm, "");
const font = (family, folder, file) => `@font-face{font-family:${family};src:url(data:font/woff2;base64,${fs.readFileSync(path.join(web, `node_modules/geist/dist/fonts/${folder}/${file}`)).toString("base64")}) format('woff2');font-weight:100 900;font-display:swap}`;
let html = fs.readFileSync(path.join(web, "src/theme-lab/template.html"), "utf8");
html = html.replace("/* PRESET_STYLES */", styles + font("Geist", "geist-sans", "Geist-Variable.woff2") + font("GeistMono", "geist-mono", "GeistMono-Variable.woff2"));
html = html.replace("/* DOCK_MODEL */", model).replace("/* PALETTES */", JSON.stringify(palettes)).replace("/* LAB_CONFIG */", JSON.stringify(config));
const output = path.resolve(process.argv[2] ?? path.join(web, "out/theme-lab.html"));
fs.mkdirSync(path.dirname(output), { recursive: true });
fs.writeFileSync(output, html);
console.log(output);
