/**
 * @nemuz/sdk — write nemuz tools in TypeScript or JavaScript.
 */

/** A JSON Schema object describing a tool's arguments. */
export interface JSONSchema {
  type: "object";
  properties?: Record<string, unknown>;
  required?: string[];
  additionalProperties?: boolean;
  [key: string]: unknown;
}

/**
 * What a tool asks to be allowed to touch.
 *
 * This is a request, not a grant. The host clamps filesystem paths to the
 * workspace and refuses network and process access the operator has not
 * approved, so declaring more than a tool uses only makes refusal likelier.
 */
export interface Capabilities {
  /** Directories to read, recursively. */
  fsRead?: string[];
  /** Directories to write, recursively. */
  fsWrite?: string[];
  /** Network destinations, as host or scheme://host. */
  net?: string[];
  /** Programs the tool needs to run. */
  exec?: string[];
}

/** Context passed to every tool call. */
export interface ToolContext {
  /** The directory the host asked this plugin to work in. */
  workspace: string;
}

/** A failed result the model sees and can recover from. */
export interface ToolFailure {
  content: string;
  isError: true;
}

/** A successful result. */
export interface ToolSuccess {
  content: string;
  isError?: false;
}

export type ToolResult = string | ToolSuccess | ToolFailure;

/** One tool a plugin provides. */
export interface Tool<Args = Record<string, unknown>> {
  /** Unique within the plugin. The host namespaces it as `plugin__tool`. */
  name: string;
  /** Shown to the model. Say what the tool does and when to reach for it. */
  description?: string;
  /** JSON Schema for the arguments. Defaults to an empty object schema. */
  schema?: JSONSchema;
  /** What the tool needs permission for. Defaults to nothing. */
  capabilities?: Capabilities;
  /**
   * Run the tool.
   *
   * Return a string for the common case. Return `toolError(message)`, or throw,
   * to report a failure the model should see and work around; the turn
   * continues either way.
   */
  run(args: Args, context: ToolContext): ToolResult | Promise<ToolResult>;
}

/** A plugin: a name, a version, and the tools it offers. */
export interface Plugin {
  name: string;
  version?: string;
  tools: Tool<any>[];
}

/**
 * Start the plugin and serve the host until it shuts down.
 *
 * Console output is redirected to stderr, because stdout carries the protocol
 * and a stray `console.log` would corrupt it.
 */
export function definePlugin(plugin: Plugin): void;

/** Build a failure the model should see, rather than throwing. */
export function toolError(message: string): ToolFailure;
