// ESLint flat config for the embedded frontend (internal/web/static/app.js),
// run by `make jscheck`. Correctness rules only: the file is vanilla script
// code in an IIFE, so every browser global it uses is listed here and
// anything else referenced but never declared is an error.
export default [{
  files: ["**/*.js"],
  languageOptions: {
    ecmaVersion: 2022,
    sourceType: "script",
    globals: {
      window: "readonly", document: "readonly", location: "readonly", history: "readonly",
      fetch: "readonly", FormData: "readonly", EventSource: "readonly", requestAnimationFrame: "readonly",
      setTimeout: "readonly", clearTimeout: "readonly", setInterval: "readonly", confirm: "readonly",
      Promise: "readonly", console: "readonly", Set: "readonly", Map: "readonly", Number: "readonly",
      String: "readonly", Math: "readonly", Date: "readonly", Object: "readonly", JSON: "readonly", Array: "readonly",
    },
  },
  rules: { "no-undef": "error", "no-unreachable": "error", "no-dupe-keys": "error", "no-unused-vars": "warn" },
}];
