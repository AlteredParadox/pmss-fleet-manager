// ESLint flat config for the embedded frontend (internal/web/static/*.js)
// and its Node test, run by `make jscheck`. Correctness rules only: the
// page files are vanilla script code in IIFEs, so every browser global they
// use is listed here and anything else referenced but never declared is an
// error.
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
      globalThis: "readonly", pfmRevisions: "readonly", // static/revisions.js defines it, app.js uses it
    },
  },
  rules: { "no-undef": "error", "no-unreachable": "error", "no-dupe-keys": "error", "no-unused-vars": "warn" },
}, {
  files: ["**/*_test.mjs"],
  languageOptions: { ecmaVersion: 2022, sourceType: "module", globals: { Map: "readonly", Set: "readonly", URL: "readonly" } },
  rules: { "no-undef": "error", "no-unreachable": "error", "no-dupe-keys": "error", "no-unused-vars": "warn" },
}];
