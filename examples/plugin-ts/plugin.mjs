// A reference nemuz plugin in JavaScript.
//
// Run it through the host with:
//   nemuz plugin call "node examples/plugin-ts/plugin.mjs" shout '{"text":"halo"}'

import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { definePlugin, toolError } from "../../sdk/typescript/index.mjs";

definePlugin({
  name: "example",
  version: "0.1.0",
  tools: [
    {
      name: "shout",
      description: "Uppercase a string. Needs nothing from the machine.",
      schema: {
        type: "object",
        properties: { text: { type: "string" } },
        required: ["text"],
        additionalProperties: false,
      },
      // No capabilities at all: pure computation, and nothing to refuse.
      run: ({ text }) => String(text).toUpperCase(),
    },
    {
      name: "count_lines",
      description: "Count the lines of a file in the workspace.",
      schema: {
        type: "object",
        properties: { path: { type: "string" } },
        required: ["path"],
        additionalProperties: false,
      },
      capabilities: { fsRead: ["."] },
      async run({ path }, { workspace }) {
        try {
          const body = await readFile(join(workspace, path), "utf8");
          return String(body.split("\n").length - 1);
        } catch (err) {
          // A missing file is the model's problem to solve, not a crash.
          return toolError(`cannot read ${path}: ${err.message}`);
        }
      },
    },
  ],
});
