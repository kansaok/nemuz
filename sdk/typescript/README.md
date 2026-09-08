# @nemuz/sdk

Write [nemuz](../..) agent tools in TypeScript or JavaScript.

A plugin is a process. nemuz starts it, speaks newline-delimited JSON-RPC over
stdio, and shuts it down when the turn ends. This package handles all of that,
so you write tool functions and nothing else.

## A whole plugin

```js
import { definePlugin, toolError } from "@nemuz/sdk";

definePlugin({
  name: "weather",
  version: "1.0.0",
  tools: [
    {
      name: "forecast",
      description: "Get tomorrow's forecast for a city.",
      schema: {
        type: "object",
        properties: { city: { type: "string" } },
        required: ["city"],
        additionalProperties: false,
      },
      capabilities: { net: ["api.open-meteo.com"] },
      async run({ city }) {
        const res = await fetch(`https://api.open-meteo.com/v1/search?name=${city}`);
        if (!res.ok) return toolError(`weather service returned ${res.status}`);
        return JSON.stringify(await res.json());
      },
    },
  ],
});
```

Run it with `nemuz`:

```bash
nemuz run "what is the weather in Jakarta?" --plugin "node ./weather.mjs"
```

## Capabilities

`capabilities` is a **request**, not a grant. The host clamps filesystem paths to
the workspace, and refuses network or process access the operator has not
approved:

```
plugin weather: tool "forecast" wants network access to "api.open-meteo.com",
which is not allowed
```

The operator approves it explicitly. Declaring more than your tool actually uses
only makes that refusal more likely, so ask for the narrowest set that works —
and prefer tools that need nothing at all.

## Returning results

| Return | Meaning |
|---|---|
| `"some text"` | Success |
| `toolError("...")` | The tool ran and failed. The model sees the message and can try something else. |
| `throw new Error(...)` | Same as `toolError`, for exceptions you did not plan for. |

A failure is not a crash: the turn continues, and the model gets a chance to
recover. Only a protocol or transport failure ends the turn.

## Rules

- **Never write to stdout.** It carries the protocol. The SDK redirects
  `console.log` to stderr for you, so logging is safe — but a raw
  `process.stdout.write` is not.
- **Keep tools deterministic where you can.** nemuz replays recorded turns to
  reproduce bugs and to test learned skills; a tool that returns the current
  time makes its turns unreplayable.
- **Name tools for what they do**, not how they work. The model reads the name
  and description and nothing else.

## Types

The package ships TypeScript definitions. `Tool<Args>` takes your argument type,
so `run` is typed:

```ts
import { definePlugin, type Tool } from "@nemuz/sdk";

const forecast: Tool<{ city: string }> = {
  name: "forecast",
  run: ({ city }) => `it is sunny in ${city}`,
};
```
