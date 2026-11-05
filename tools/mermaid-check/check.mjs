// Syntax check of a Mermaid diagram with the real Mermaid parser.
//
// Reads the diagram from stdin. Exit 0 and no output: it parses. Exit 1: a
// syntax error, the parser message on stdout. Exit 2: the check itself failed
// (message on stderr). Runs in the sandbox image (Dockerfile.sandbox), without
// network and credentials; mermaid and jsdom are installed in
// /opt/mermaid-check. Only parse() runs, nothing is rendered.
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";

const root = process.env.MERMAID_CHECK_DIR || "/opt/mermaid-check";
const require = createRequire(root + "/");

try {
  const { JSDOM } = require("jsdom");
  const dom = new JSDOM("<body></body>");
  globalThis.window = dom.window;
  globalThis.document = dom.window.document;
  const mermaid = (await import(pathToFileURL(require.resolve("mermaid")).href)).default;

  let src = "";
  for await (const chunk of process.stdin) src += chunk;
  try {
    await mermaid.parse(src.trim());
  } catch (e) {
    console.log(String((e && e.message) || e).slice(0, 2000));
    process.exit(1);
  }
} catch (e) {
  console.error(String((e && e.stack) || e));
  process.exit(2);
}
process.exit(0);
