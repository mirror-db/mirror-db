/** A key name the provider does not know — a client error, not a failure. */
export class UnknownKeyError extends Error {
  constructor(name: string) {
    super(`unknown key: ${name}`);
    this.name = "UnknownKeyError";
  }
}
