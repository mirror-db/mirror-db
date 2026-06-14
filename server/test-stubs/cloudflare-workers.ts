// Test stub for the `cloudflare:workers` virtual module. Vitest aliases
// `cloudflare:workers` to this file so modules that read `env` can be imported
// in a plain Node environment. Tests mutate `env` before exercising handlers.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export const env: any = {};

// noop — in tests cache writes don't need lifecycle extension.
export function waitUntil(_promise: Promise<unknown>): void {}
