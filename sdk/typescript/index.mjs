/**
 * @nemuz/sdk — write nemuz tools in JavaScript or TypeScript.
 *
 * A plugin is a process that reads newline-delimited JSON-RPC on stdin and
 * writes it on stdout. This module handles that entirely, so a plugin author
 * writes tool functions and nothing else.
 */

const PROTOCOL_VERSION = 1;

// JSON-RPC codes. TOOL_FAILED tells the host the tool ran and failed, which the
// host passes back to the model so it can try something else. Every other code
// means the plugin itself misbehaved, which aborts the turn.
const CODE_INVALID_PARAMS = -32602;
const CODE_METHOD_NOT_FOUND = -32601;
const CODE_TOOL_FAILED = -32000;

/**
 * Redirect console output to stderr.
 *
 * stdout is the protocol channel. A single stray console.log corrupts the
 * stream and the host drops the plugin with a parse error — historically the
 * most common way a working plugin breaks. Rather than warn about it in the
 * docs, the SDK makes it impossible: console.log still works, it just goes
 * where the host can read it.
 *
 * This runs when the module is imported, not when definePlugin is called.
 * Module-level logging in the plugin — or in anything it imports — happens
 * before definePlugin runs, and that is exactly when a plugin is most likely
 * to announce itself.
 */
function protectStdout() {
  const toStderr =
    (level) =>
    (...args) => {
      const text = args
        .map((a) => (typeof a === "string" ? a : safeStringify(a)))
        .join(" ");
      process.stderr.write(`[${level}] ${text}\n`);
    };
  console.log = toStderr("log");
  console.info = toStderr("info");
  console.warn = toStderr("warn");
  console.error = toStderr("error");
  console.debug = toStderr("debug");
}

function safeStringify(value) {
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

protectStdout();

/**
 * Start a plugin. Blocks until the host asks it to shut down.
 *
 * @param {object} plugin
 * @param {string} plugin.name
 * @param {string} [plugin.version]
 * @param {Array} plugin.tools
 */
export function definePlugin(plugin) {
  validate(plugin);

  const byName = new Map(plugin.tools.map((t) => [t.name, t]));
  /** Set from the host's initialize call; tools read it via their context. */
  let workspace = "";

  const write = (message) => {
    process.stdout.write(JSON.stringify(message) + "\n");
  };

  const handle = async (request) => {
    switch (request.method) {
      case "initialize": {
        workspace = request.params?.workspace ?? "";
        return {
          protocol: PROTOCOL_VERSION,
          name: plugin.name,
          version: plugin.version ?? "0.0.0",
          tools: plugin.tools.map((t) => ({
            name: t.name,
            description: t.description ?? "",
            schema: t.schema ?? { type: "object" },
            // Declared capabilities are a request. The host decides what is
            // actually granted, so asking for more than a tool uses only
            // makes refusal more likely.
            capabilities: t.capabilities ?? {},
          })),
        };
      }

      case "tools/call": {
        const name = request.params?.name;
        const tool = byName.get(name);
        if (!tool) {
          throw rpcError(CODE_INVALID_PARAMS, `no tool named ${JSON.stringify(name)}`);
        }
        const result = await tool.run(request.params?.arguments ?? {}, { workspace });
        return normaliseResult(result);
      }

      default:
        throw rpcError(CODE_METHOD_NOT_FOUND, `unknown method ${request.method}`);
    }
  };

  readLines(process.stdin, async (line) => {
    let request;
    try {
      request = JSON.parse(line);
    } catch (err) {
      process.stderr.write(`unreadable request: ${err.message}\n`);
      return;
    }
    if (request.method === "shutdown") {
      process.exit(0);
    }

    let response;
    try {
      response = { jsonrpc: "2.0", id: request.id, result: await handle(request) };
    } catch (err) {
      response = {
        jsonrpc: "2.0",
        id: request.id,
        // A thrown error means the tool failed, which the model should see.
        error: err?.rpc ?? { code: CODE_TOOL_FAILED, message: err?.message ?? String(err) },
      };
    }
    // Notifications carry no id and expect no reply.
    if (request.id !== undefined && request.id !== null) {
      write(response);
    }
  });
}

/** Normalise what a tool returned into a wire result. */
function normaliseResult(result) {
  if (typeof result === "string") {
    return { content: result };
  }
  if (result && typeof result === "object" && "content" in result) {
    return { content: String(result.content), isError: Boolean(result.isError) };
  }
  // Anything else is rendered rather than silently becoming "[object Object]".
  return { content: safeStringify(result) };
}

/** Build an error that carries an explicit JSON-RPC code. */
function rpcError(code, message) {
  const err = new Error(message);
  err.rpc = { code, message };
  return err;
}

/**
 * Report a tool failure the model should see and recover from, as opposed to
 * throwing an unexpected error.
 */
export function toolError(message) {
  return { content: message, isError: true };
}

function validate(plugin) {
  if (!plugin?.name) {
    throw new Error("definePlugin: a plugin needs a name");
  }
  if (!Array.isArray(plugin.tools) || plugin.tools.length === 0) {
    throw new Error("definePlugin: a plugin needs at least one tool");
  }
  const seen = new Set();
  for (const tool of plugin.tools) {
    if (!tool?.name) throw new Error("definePlugin: every tool needs a name");
    if (typeof tool.run !== "function") {
      throw new Error(`definePlugin: tool ${tool.name} has no run function`);
    }
    if (seen.has(tool.name)) {
      throw new Error(`definePlugin: tool ${tool.name} is declared twice`);
    }
    seen.add(tool.name);
  }
}

/**
 * Read stdin as newline-delimited records, handling each in order.
 *
 * Requests are processed one at a time even when a handler is async: the host
 * sends them serially, and preserving that order keeps a slow tool from
 * reordering replies.
 */
function readLines(stream, onLine) {
  let buffer = "";
  let queue = Promise.resolve();

  stream.setEncoding("utf8");
  stream.on("data", (chunk) => {
    buffer += chunk;
    let index;
    while ((index = buffer.indexOf("\n")) >= 0) {
      const line = buffer.slice(0, index).trim();
      buffer = buffer.slice(index + 1);
      if (line) {
        queue = queue.then(() => onLine(line));
      }
    }
  });
  stream.on("end", () => {
    queue.then(() => process.exit(0));
  });
}
