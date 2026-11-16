
globalThis.XMLHttpRequestExtension_send_native = function(method, url, headers, body, callback, xhr) {
  const requestHeaders = {};
  headers.forEach(pair => { requestHeaders[pair[0]] = pair[1]; });
  xhr.__lxAbort = lx.request(url, {
    method: method,
    headers: requestHeaders,
    body: body,
    timeout: xhr.timeout,
    json: false
  }, function(error, response, text) {
    xhr.__lxAbort = null;
    callback({
      statusCode: response ? response.statusCode : 0,
      statusText: response ? response.statusMessage : '',
      responseHeaders: response ? Object.entries(response.headers || {}) : []
    }, text, error ? (/timeout/i.test(String(error)) ? 'timeout' : String(error)) : null);
  });
};
XMLHttpRequest.prototype.abort = function() {
  const active = !!this.__lxAbort;
  if (active) this.__lxAbort();
  this.__lxAbort = null;
  this.readyState = XMLHttpRequest.UNSENT;
  this.status = 0;
  if (active && typeof this.onabort === 'function') this.onabort();
  if (active && typeof this.onloadend === 'function') this.onloadend();
};
XMLHttpRequest.prototype.getResponseHeader = function(name) {
  const matches = this._responseHeaders.filter(pair =>
    pair[0].toLowerCase() === String(name).toLowerCase());
  return matches.length ? matches.map(pair => pair[1]).join(', ') : null;
};
