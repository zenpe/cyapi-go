// Development-only regression tests; the Go service does not require Node.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function fixture(initialRelease, reportResponse) {
  const elements = new Map(), requests = [];
  const makeElement = () => ({
    value: '', listeners: {}, children: [], classList: { toggle() {} },
    set textContent(value) { this.text = String(value); this.children = []; },
    get textContent() { return (this.text || '') + this.children.map(child => child.textContent).join(''); },
    addEventListener(type, handler) { this.listeners[type] = handler; },
    showModal() { this.open = true; }, close() { this.open = false; },
    querySelector() { return { disabled: false }; },
    append(...children) { this.children.push(...children); },
    replaceChildren(...children) { this.text = ''; this.children = children; },
  });
  const element = id => {
    if (!elements.has(id)) elements.set(id, makeElement());
    return elements.get(id);
  };
  const state = id => ({ current: { releaseId: id, originalName: id, scriptSha256: 'a'.repeat(64) }, history: [], enabled: true });
  let current = state(initialRelease);
  const context = {
    document: { getElementById: element, createElement: makeElement, addEventListener() {}, hidden: false },
    location: { pathname: '/admin/source-management' },
    setTimeout() { return 1; }, clearTimeout() {}, AbortController,
    fetch: async (url, options) => {
      if (url.includes('/source-checks/runs/')) return { ok: true, json: async () => reportResponse };
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
    'globalThis.review = {openPublish,loadRecommendations,healthCell,showReport};})();',
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

test('failed attempts and probe limitations remain visible in overview and publication', async () => {
  const f = fixture('release-A');
  const health = { Failed: 3, Limited: 2, Recognized: 0, Short: 0, Unverified: 0, Unsupported: 0 };
  const cell = f.api.healthCell(health, 'test.js');
  assert.match(cell.textContent, /3 失败/);
  assert.match(cell.textContent, /失败中 2 项涉及检测限制/);
  assert.doesNotMatch(cell.textContent, /未验证/);
  await f.api.loadRecommendations();
  f.api.openPublish({ name: 'test', sha256: 'a'.repeat(64), platforms: { qq: health } });
  assert.match(f.element('publish-health').textContent, /3 失败（其中 2 项涉及检测限制）/);
});

test('report expands evidence and distinguishes an unexecuted resource from an unknown format', async () => {
  const result = (id, resolution, resource, more = {}) => ({ sampleId: id, scriptPath: 'test.js', platform: 'qq', initialization: 'ok', resolution, resource, ...more });
  const response = { currentProbeVersion: 'goja-probe-v2', report: {
    probeVersion: 'goja-probe-v2', completed: 4, planned: 4, state: 'completed', samples: [], results: [
      result('failed', 'failed', { status: 'not_run' }, { outcome: 'failed', reason: 'QQ 无播放权限(104003)', probeIssues: ['probe_policy: resolved address 198.18.0.1'] }),
      result('unknown', 'ok', { status: 'unverified', bytes: 100, contentType: 'application/octet-stream' }, { outcome: 'unverified' }),
      result('missing', 'ok', { status: 'unreachable', httpStatus: 404 }, { outcome: 'failed' }),
      result('aac', 'ok', { status: 'recognized_audio', format: 'm4a', codec: 'aac', durationMs: 217129, durationMethod: 'mp4_mdhd', complete: true, sha256: 'b'.repeat(64) }, { outcome: 'recognized_audio' }),
    ],
  } };
  const f = fixture('', response);
  await f.api.showReport('report-1');
  const cards = f.element('report-results').children.filter(x => x.className === 'result-card');
  assert.equal(cards.length, 4);
  assert.ok(cards.every(card => card.open));
  assert.match(cards[0].textContent, /解析失败.*资源未执行/);
  assert.match(cards[0].textContent, /198\.18\.0\.1/);
  assert.match(cards[0].textContent, /104003/);
  assert.doesNotMatch(cards[0].textContent, /资源未验证/);
  assert.match(cards[1].textContent, /资源未验证/);
  assert.match(cards[2].textContent, /HTTP 404/);
  assert.match(cards[3].textContent, /M4A 音轨元数据/);
  assert.match(cards[3].textContent, /编码 aac/);
  assert.match(cards[3].textContent, /完整资源 SHA256 b{64}/);
  response.report.probeVersion = 'goja-probe-v1';
  await f.api.showReport('report-1');
  assert.match(f.element('report-results').textContent, /旧版报告/);
});

test('resource warnings stay separate from outcomes and decoding claims', async () => {
  const response = { currentProbeVersion: 'goja-probe-v5', report: {
    probeVersion: 'goja-probe-v5', completed: 3, planned: 3, state: 'completed',
    samples: [{id:'a',title:'Song A'},{id:'b',title:'Song B'},{id:'c',title:'Song C'}],
    results: [
      {sampleId:'a',scriptPath:'test.js',platform:'qq',initialization:'ok',resolution:'ok',outcome:'suspected_short_resource',resource:{status:'recognized_audio',assessment:'suspected_short_resource',duplicateSampleIds:['b'],durationMs:7128}},
      {sampleId:'b',scriptPath:'test.js',platform:'qq',initialization:'ok',resolution:'ok',outcome:'suspected_short_resource',resource:{status:'recognized_audio',assessment:'suspected_short_resource',duplicateSampleIds:['a'],durationMs:7128}},
      {sampleId:'c',scriptPath:'test.js',platform:'qq',initialization:'ok',resolution:'ok',outcome:'unverified',resource:{status:'unverified',reasonCode:'suspected_encrypted_resource',extension:'.mgg',headerHex:'7d35280165'}},
    ],
  }};
  const f=fixture('',response);
  await f.api.showReport('report-1');
  const summary=f.element('report-results').children.find(x=>x.className==='report-summary');
  assert.match(summary.textContent,/2 项结构可识别（其中 2 项疑似短音频）/);
  assert.match(summary.textContent,/0 项失败，1 项资源未验证（其中 1 项疑似加密）/);
  assert.match(summary.textContent,/2 项与其他歌曲样例返回相同完整文件/);
  const cards=f.element('report-results').children.filter(x=>x.className==='result-card');
  assert.match(cards[0].textContent,/与 Song B 返回相同完整文件/);
  assert.match(cards[0].textContent,/格式：结构已识别 · 时长：疑似短音频 · 解码：未执行/);
  assert.match(cards[2].textContent,/疑似加密资源/);
  assert.match(cards[2].textContent,/资源后缀 .mgg · 文件头 7d35280165/);
  assert.doesNotMatch(cards[2].textContent,/检测失败|已通过|解码成功/);
  const health={Recognized:2,Short:0,Failed:0,Unverified:1,Duplicate:2,SuspectedEncrypted:1};
  assert.match(f.api.healthCell(health,'test.js').textContent,/其中 2 项重复资源，需核对/);
  assert.match(f.api.healthCell(health,'test.js').textContent,/未验证中 1 项疑似加密/);
  await f.api.loadRecommendations();
  f.api.openPublish({name:'test',sha256:'a'.repeat(64),platforms:{qq:health}});
  assert.match(f.element('publish-health').textContent,/其中 2 项重复资源，需核对/);
});
