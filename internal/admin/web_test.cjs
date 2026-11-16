// Development-only regression tests; the Go service does not require Node.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function fixture(initialRelease) {
  const elements = new Map(), requests = [];
  const element = id => {
    if (!elements.has(id)) elements.set(id, {
      value: '', listeners: {}, classList: { toggle() {} },
      addEventListener(type, handler) { this.listeners[type] = handler; },
      showModal() { this.open = true; }, close() { this.open = false; },
      querySelector() { return { disabled: false }; }, replaceChildren() {},
    });
    return elements.get(id);
  };
  const state = id => ({ current: { releaseId: id, originalName: id, scriptSha256: 'a'.repeat(64) }, history: [], enabled: true });
  let current = state(initialRelease);
  const context = {
    document: { getElementById: element, addEventListener() {}, hidden: false },
    location: { pathname: '/admin/source-management' },
    setTimeout() { return 1; }, clearTimeout() {}, AbortController,
    fetch: async (_, options) => {
      if (options.method !== 'POST') return { ok: true, json: async () => current };
      const body = JSON.parse(options.body);
      requests.push(body);
      if (body.expectedReleaseId !== current.current.releaseId) {
        return { ok: false, status: 409, json: async () => ({ message: 'version conflict' }) };
      }
      current = state('published-target');
      return { ok: true, json: async () => ({ reused: false }) };
    },
  };
  vm.createContext(context);
  // Expose closure entry points only in this VM; production exports stay unchanged.
  const script = fs.readFileSync(path.join(__dirname, 'web/app.js'), 'utf8').replace(
    /\}\)\(\);\s*$/,
    'globalThis.review = {openPublish,loadRecommendations};})();',
  );
  vm.runInContext(script, context);
  return {
    element, requests, api: context.review,
    changeRelease(id) { current = state(id); },
    currentRelease() { return current.current.releaseId; },
    async submit() {
      element('publish-note').value = 'confirmed by administrator';
      await element('publish-form').listeners.submit({
        preventDefault() {}, currentTarget: element('publish-form'),
      });
    },
  };
}

for (const rollback of [false, true]) {
  test(`confirmation preserves its expected version across refresh (rollback=${rollback})`, async () => {
    const f = fixture('release-A');
    await f.api.loadRecommendations();
    const target = { name: 'B', originalName: 'B', sha256: 'b'.repeat(64), scriptSha256: 'b'.repeat(64), path: 'B.js', generation: 'g-1', releaseId: 'historical-B' };
    f.api.openPublish(target, rollback);
    f.changeRelease('release-C');
    await f.api.loadRecommendations();
    await f.submit();
    assert.equal(f.requests[0].expectedReleaseId, 'release-A');
    assert.equal(f.currentRelease(), 'release-C');
    assert.equal(f.element('publish-dialog').open, true);
    assert.match(f.element('publish-error').textContent, /重新选择/);
    // Retrying the stale dialog must still conflict, even after refresh.
    await f.submit();
    assert.equal(f.requests[1].expectedReleaseId, 'release-A');
    f.element('publish-dialog').close();
    f.api.openPublish(target, rollback);
    await f.submit();
    assert.equal(f.requests[2].expectedReleaseId, 'release-C');
    assert.equal(f.currentRelease(), 'published-target');
    assert.equal(f.element('publish-dialog').open, false);
  });
}

test('first publication preserves the empty expected version', async () => {
  const f = fixture('');
  await f.api.loadRecommendations();
  f.api.openPublish({ name: 'A', sha256: 'a'.repeat(64), path: 'A.js', generation: 'g-1' });
  await f.submit();
  assert.equal(f.requests[0].expectedReleaseId, '');
  assert.equal(f.currentRelease(), 'published-target');
});
