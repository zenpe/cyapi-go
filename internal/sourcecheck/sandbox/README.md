# LX compatibility scripts

Copied without source-script rewriting from the validated Goja prototype in
CyreneMusic `artifacts/goja-source-probe`, 2026-11-13.

- `sandbox.js`: client `lib/services/lx_sandbox_js.dart` JavaScript body.
- `browser.js`: client `lib/services/lx_browser_transport_js.dart` JavaScript body.
- `xhr.js` and `fetch.js`: client-vendored flutter_js compatibility implementations;
  see `LICENSE.flutter_js` for the upstream MIT license.

Go implements the native bridge. These copies do not automatically track future
client changes. Any bridge update must run the async/HTTP, encoding/crypto and
cancellation contract tests and update `ProbeVersion`. Passing these contracts
does not prove complete QuickJS/Goja equivalence.
