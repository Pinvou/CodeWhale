// Fault injection is confined to a test child started with Node --import.
const originalWarn = console.warn;
let attempts = 0;

console.warn = (...args) => {
  if (args[0] === "[relay-capacity] legacy_revocations") {
    attempts += 1;
    throw new Error("fixture legacy warning sink unavailable");
  }
  Reflect.apply(originalWarn, console, args);
};

process.on("message", (message) => {
  if (message?.type === "read-legacy-warning-attempts") {
    process.send?.({ type: "legacy-warning-attempts", attempts });
  }
});
