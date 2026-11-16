'use strict';
(() => {
  const $ = id => document.getElementById(id);
  const base = location.pathname.replace(/\/admin\/source-management\/?$/, '');
  const sessionKey = `cyapi.admin.token:${base || '/'}`;
  let recommendationState=null,publishTarget=null;
  let token = '', samples = [], sampleVersion = '', overview = null, timer = null, selectedReport = '', selectedPath = '', generation = 0, busy = false, polling = false;
  const platformName = p => p === 'qq' ? 'QQ 音乐' : '网易云';
  const stateName = s => ({running:'检测中',completed:'已完成',canceled:'已取消',interrupted:'已中断',failed:'执行失败'}[s] || s);
  const date = s => s ? new Date(s).toLocaleString('zh-CN',{hour12:false}) : '—';
  const duration = ms => ms == null ? '未核实' : `${Math.floor(ms/60000)}:${String(Math.floor(ms/1000)%60).padStart(2,'0')}`;
  const outcomeName = s => ({failed:'检测失败',unverified:'资源未验证',recognized_audio:'资源结构可识别',suspected_short_resource:'疑似短音频',unsupported:'不支持',canceled:'已取消'}[s]||s);
  const stageName = s => ({probe:'检测执行',sample:'测试样例',initialization:'初始化',resolution:'地址解析',resource:'资源检查'}[s]||s);
  const originName = s => ({probe:'检测环境或限制',sample:'测试样例',source:'脚本或上游接口',resource:'返回的资源'}[s]||s);
  const durationMethod = s => ({mp4_mdhd:'M4A 音轨元数据',mp3_xing:'MP3 Xing 帧数',mp3_vbri:'MP3 VBRI 帧数',mp3_complete_frames:'完整 MP3 逐帧时长',flac_streaminfo:'FLAC STREAMINFO',ogg_eos_granule:'完整 Ogg 结束页采样位置'}[s]||s||'尚无可靠时长依据');
  const resourceReasonName = s => ({suspected_encrypted_resource:'疑似加密资源',format_evidence_incomplete:'读取范围内证据不足',unsupported_or_unknown_format:'暂不支持或未知格式',ogg_evidence_incomplete:'Ogg 音轨证据不足',ogg_codec_unsupported:'Ogg 编码暂未支持',ogg_multiple_streams_unsupported:'Ogg 多逻辑流暂未支持',ogg_invalid_structure:'Ogg 结构异常',ogg_checksum_mismatch:'Ogg 页校验失败',ogg_page_limit:'Ogg 检查预算不足',ogg_identification_limit:'Ogg 标识包超过预算'}[s]||s);
  const durationAssessment = r => r.assessment==='suspected_short_resource'?'疑似短音频':r.assessment==='no_short_duration_detected'?'未发现明显偏短':'未核实';
  function reportSummary(results) {
    const summary = node('div', null, 'report-summary');
    const outcome = result => {
      if (result.outcome) return result.outcome;
      if (result.resolution === 'canceled') return 'canceled';
      if (result.initialization === 'unsupported') return 'unsupported';
      if (result.resolution !== 'ok') return 'failed';
      if (result.resource.status === 'recognized_audio') {
        return result.resource.assessment === 'suspected_short_resource'
          ? 'suspected_short_resource' : 'recognized_audio';
      }
      return result.resource.status === 'unverified' ? 'unverified' : 'failed';
    };
    for (const platform of ['qq', 'netease']) {
      const rows = results.filter(result => result.platform === platform);
      if (!rows.length) continue;
      const count = predicate => rows.filter(predicate).length;
      const recognized = count(result => result.resource.status === 'recognized_audio');
      const short = count(result => outcome(result) === 'suspected_short_resource');
      const failed = count(result => outcome(result) === 'failed');
      const unknown = count(result => outcome(result) === 'unverified');
      const encrypted = count(result => result.resource.reasonCode === 'suspected_encrypted_resource');
      const duplicate = count(result => result.resource.duplicateSampleIds?.length);
      const unsupported = count(result => outcome(result) === 'unsupported');
      const canceled = count(result => outcome(result) === 'canceled');
      const text = `${platformName(platform)}：${recognized} 项结构可识别`
        + (short ? `（其中 ${short} 项疑似短音频）` : '')
        + `，${failed} 项失败，${unknown} 项资源未验证`
        + (encrypted ? `（其中 ${encrypted} 项疑似加密）` : '')
        + (unsupported ? `，${unsupported} 项不支持` : '')
        + (canceled ? `，${canceled} 项已取消` : '')
        + (duplicate ? `；${duplicate} 项与其他歌曲样例返回相同完整文件，需核对` : '');
      summary.append(node('p', text));
    }
    summary.append(node('p', '本报告检查资源结构和时长，未进行解码验证，也未确认歌曲内容身份。', 'muted small'));
    return summary;
  }
  function node(tag, text, cls) { const e=document.createElement(tag); if(text!=null)e.textContent=text;if(cls)e.className=cls;return e; }
  function button(text, action, cls='quiet') {const e=node('button',text,cls);e.type='button';e.addEventListener('click',action);return e;}
  function notice(text,error=false){$('notice').textContent=text;$('notice').classList.toggle('is-error',error);$('notice').hidden=!text;}
  function savedToken(){try{return sessionStorage.getItem(sessionKey)||'';}catch{return '';}}
  function saveToken(){try{if(token)sessionStorage.setItem(sessionKey,token);else sessionStorage.removeItem(sessionKey);}catch{}}
  function logout(){generation++;token='';saveToken();overview=null;clearTimeout(timer);$('workspace').hidden=true;$('identity').hidden=true;$('login-panel').hidden=false;if($('report-dialog').open)$('report-dialog').close();$('password').value='';recommendationState=null;if($('publish-dialog').open)$('publish-dialog').close();}
  async function openSession(username){await loadSamples();saveToken();$('username').textContent=username;$('identity').hidden=false;$('login-panel').hidden=true;$('workspace').hidden=false;$('password').value='';await refresh();}
  async function restoreSession(){token=savedToken();if(!token)return;const submit=$('login-form').querySelector('button');submit.disabled=true;const label=submit.textContent;submit.textContent='正在恢复登录…';try{const data=await api('/auth/validate-token');await openSession(data.data.username);}catch(e){token='';$('login-error').textContent=e.status===401?'登录已过期，请重新登录':e.status===403?'当前账号没有管理权限':`恢复登录失败：${e.message}。可刷新重试或重新登录`;}finally{submit.disabled=false;submit.textContent=label;}}
  async function api(path,options={}) {
    const current=generation, headers={'Accept':'application/json',...options.headers};if(token)headers.Authorization=`Bearer ${token}`;if(options.body)headers['Content-Type']='application/json';
    const controller=new AbortController(),timeout=setTimeout(()=>controller.abort(),25000);
    let response;try{response=await fetch(base+path,{...options,headers,cache:'no-store',credentials:'omit',signal:controller.signal});}finally{clearTimeout(timeout);}
    let data;try{data=await response.json();}catch{throw Error('服务器返回了无法识别的响应');}
    if(current!==generation)throw Error('会话已改变');
    if(!response.ok){if((response.status===401||response.status===403)&&token){logout();$('login-error').textContent=response.status===401?'登录已过期，请重新登录':'当前账号没有管理权限';}const error=Error(data.message||`请求失败（${response.status}）`);error.status=response.status;throw error;}
    return data;
  }
  async function action(work){if(busy)return;busy=true;try{await work();}catch(e){notice(e.message,true);if(e.status===409){try{await loadSamples();}catch{}}}finally{busy=false;renderSources();}}
  async function loadSamples(){const data=await api('/admin/source-checks/samples');samples=data.samples;sampleVersion=data.version;renderSamples();}
  async function refresh(){if(!token||polling)return;polling=true;try{overview=await api('/admin/source-checks/overview');if(overview.sampleVersion!==sampleVersion)await loadSamples();await loadRecommendations();renderSources();renderTask();renderHistory();$('warning').textContent=overview.warning||'';$('warning').hidden=!overview.warning;}catch(e){if(token)notice(e.message,true);}finally{polling=false;schedule();}}
  function schedule(){clearTimeout(timer);if(token)timer=setTimeout(refresh,document.hidden?10000:overview?.activeRunId?2000:10000);}
  function healthCell(health,path){const td=node('td');if(!health){td.append(node('span','待检测','badge'));return td;}const badges=node('div',null,'health');
    for(const [key,label,cls] of [['Recognized','结构可识别','good'],['Short','疑似短音频','warn'],['Failed','失败','bad'],['Unverified','资源未验证',''],['Unsupported','不支持',''],['Canceled','已取消','']])if(health[key])badges.append(node('span',`${health[key]} ${label}`,`badge ${cls}`));
    if(health.SuspectedEncrypted)badges.append(node('span',`未验证中 ${health.SuspectedEncrypted} 项疑似加密`,'badge warn'));
    if(health.Duplicate)badges.append(node('span',`其中 ${health.Duplicate} 项重复资源，需核对`,'badge warn'));
    if(health.Limited)badges.append(node('span',`失败中 ${health.Limited} 项涉及检测限制`,'badge warn'));
    td.append(badges);if(health.Stale)td.append(node('div','结果已过期','subtext'));td.append(button(date(health.CheckedAt),()=>showReport(health.RunID,path),'quiet small'));return td;}
  function renderSources(){if(!overview)return;const q=$('search').value.trim().toLowerCase(),rows=overview.sources.filter(x=>(x.name+' '+x.path).toLowerCase().includes(q));$('source-rows').replaceChildren();
    $('source-count').textContent=overview.sources.length;$('tested-count').textContent=overview.sources.filter(x=>Object.keys(x.platforms).length>0).length;$('sample-count').textContent=samples.filter(x=>x.enabled).length;
    $('check-all').disabled=busy||!!overview.activeRunId||!overview.sources.length||!samples.some(x=>x.enabled);$('source-empty').hidden=rows.length>0;
    for(const row of rows){const tr=node('tr'),name=node('td');name.append(node('div',row.name,'source-name'),node('div',row.path,'subtext'));tr.append(name,healthCell(row.platforms.qq,row.path),healthCell(row.platforms.netease,row.path));const ops=node('td'),group=node('div',null,'actions');const check=button('检测',()=>start(row.path),'secondary');check.disabled=busy||!!overview.activeRunId||!samples.some(x=>x.enabled);group.append(check);group.append(button(recommendationState?.current?.scriptSha256===row.sha256?'已推荐':'设为推荐',()=>openPublish(row)));
      const report=row.platforms.qq?.RunID||row.platforms.netease?.RunID;if(report)group.append(button('详情',()=>showReport(report,row.path)));ops.append(group);tr.append(ops);$('source-rows').append(tr);}
  }
  function renderTask(){const id=overview?.activeRunId,run=overview?.runs.find(x=>x.runId===id);$('task-panel').hidden=!id;if(!id)return;$('task-title').textContent='正在检测';$('task-count').textContent=run?`${run.completed} / ${run.planned}`:'正在准备…';$('task-progress').max=run?.planned||1;$('task-progress').value=run?.completed||0;$('cancel-task').disabled=false;}
  function renderHistory(){$('history-list').replaceChildren();if(!overview.runs.length){$('history-list').append(node('p','暂无检测记录。选择音源并运行首次检测。','muted'));return;}
    for(const run of overview.runs){const row=node('div',null,'history-row'),meta=node('div');meta.append(node('span',stateName(run.state),'badge'),node('span',`${run.completed}/${run.planned}`),node('span',date(run.startedAt),'muted small'));row.append(meta,button('查看报告',()=>showReport(run.runId)));$('history-list').append(row);}}
  async function loadRecommendations(){const data=await api('/admin/source-recommendations');recommendationState=data;const current=data.current;$('recommendation-current').textContent=current.releaseId?`${current.originalName} · ${current.scriptSha256.slice(0,12)} · ${date(current.publishedAt)}`:'暂未发布推荐音源';$('recommendation-note').textContent=(data.enabled?'':'公共推荐入口尚未启用，请在服务配置启用 source_recommendation.enabled。 ')+(current.note||'');$('recommendation-history').replaceChildren();for(const item of data.history){const row=node('div',null,'history-row');row.append(node('span',`${item.originalName} · ${item.scriptSha256.slice(0,12)} · ${date(item.publishedAt)}`));if(item.releaseId!==current.releaseId)row.append(button('回退到此版本',()=>openPublish(item,true)));$('recommendation-history').append(row);}}
  function openPublish(target,rollback=false){if(busy||!recommendationState)return;const current=recommendationState.current;publishTarget={target,rollback,expectedReleaseId:current.releaseId||''};$('publish-target').textContent=`${current.originalName||'未发布'} → ${rollback?target.originalName:target.name} · ${(rollback?target.scriptSha256:target.sha256).slice(0,12)}`;const platforms=target.platforms||{};$('publish-health').textContent=rollback?'将归档的历史版本作为一次新发布。历史检测可能已过期。':Object.keys(platforms).length?['qq','netease'].map(p=>{const h=platforms[p];return !h?`${platformName(p)}：未检测`:`${platformName(p)}：${h.Recognized} 资源可识别，${h.Short} 疑似短资源，${h.Failed} 失败${h.Limited?`（其中 ${h.Limited} 项涉及检测限制）`:``}，${h.Unverified} 资源未验证，${h.Unsupported} 不支持，${h.Canceled||0} 已取消${h.SuspectedEncrypted?`；未验证中 ${h.SuspectedEncrypted} 项疑似加密`:""}${h.Duplicate?`；其中 ${h.Duplicate} 项重复资源，需核对`:''}${h.Stale?'（已过期）':''}`;}).join('；'):'此版本尚无匹配检测记录。发布不会自动验证客户端可播放性。';$('publish-note').value='';$('publish-error').textContent='';$('publish-dialog').showModal();}
  $('close-publish').addEventListener('click',()=>$('publish-dialog').close());
  $('publish-form').addEventListener('submit',async event=>{event.preventDefault();if(!publishTarget||busy)return;busy=true;const submit=event.currentTarget.querySelector('[type=submit]');submit.disabled=true;try{const {target,rollback,expectedReleaseId}=publishTarget;const body={expectedReleaseId,sha256:rollback?target.scriptSha256:target.sha256,note:$('publish-note').value.trim(),...(rollback?{fromReleaseId:target.releaseId}:{generation:target.generation,path:target.path})};const data=await api('/admin/source-recommendations',{method:'POST',body:JSON.stringify(body)});$('publish-dialog').close();notice(data.reused?'当前已推荐该版本':'推荐已发布，客户端启动检查时可获取此版本');await refresh();}catch(e){$('publish-error').textContent=e.status===409?'推荐或音源版本已变化，请关闭此窗口后重新选择并确认。':e.message;if(e.status===409)await refresh();}finally{busy=false;submit.disabled=false;renderSources();}});
  async function start(path=''){await action(async()=>{const data=await api('/admin/source-checks/runs',{method:'POST',body:JSON.stringify({path,platform:$('platform').value})});notice(data.reused?'已显示正在运行的相同检测任务':'检测已开始，关闭页面后仍会继续');await refresh();});}
  async function mutateSample(id,body,method='PATCH'){await action(async()=>{const data=await api('/admin/source-checks/samples/'+encodeURIComponent(id),{method,body:JSON.stringify({expectedVersion:sampleVersion,...body})});samples=data.samples;sampleVersion=data.version;renderSamples();notice('测试歌曲已更新，下次检测使用新样例');await refresh();});}
  function renderSamples(){$('sample-rows').replaceChildren();$('sample-empty').hidden=samples.length>0;for(const sample of samples){const tr=node('tr'),name=node('td');name.append(node('div',sample.title,'source-name'),node('div',`${sample.musicInfo.artist||''} · ${sample.trackId}`,'subtext'));tr.append(name,node('td',platformName(sample.platform)),node('td',duration(sample.expectedDurationMs)));
      const state=node('td'),label=node('label',null,'switch-label'),toggle=node('input');toggle.type='checkbox';toggle.checked=sample.enabled;toggle.setAttribute('aria-label',`${sample.enabled?'停用':'启用'} ${sample.title}`);toggle.addEventListener('change',()=>{const enabled=toggle.checked;toggle.checked=sample.enabled;mutateSample(sample.id,{enabled});});label.append(toggle,node('span',sample.enabled?'已启用':'已停用'));state.append(label);tr.append(state);
      const ops=node('td'),group=node('div',null,'actions');group.append(button('刷新详情',()=>mutateSample(sample.id,{action:'refresh'})),button('删除',()=>{if(confirm(`删除测试歌曲“${sample.title}”？已有报告不受影响。`))mutateSample(sample.id,{},'DELETE');},'quiet danger'));ops.append(group);tr.append(ops);$('sample-rows').append(tr);}if(overview)renderSources();}
  async function showReport(id,path=''){selectedReport=id;selectedPath=path;$('report-title').textContent=path?'音源检测详情':'检测报告';$('report-results').replaceChildren(node('p','正在加载…','muted'));if(!$('report-dialog').open)$('report-dialog').showModal();try{const data=await api('/admin/source-checks/runs/'+encodeURIComponent(id));if(selectedReport!==id)return;const report=data.report;$('report-meta').textContent=`${stateName(report.state)} · ${report.completed}/${report.planned} · ${date(report.startedAt)} · ${report.probeVersion||'旧版检测器'}`;$('report-results').replaceChildren();if(data.currentProbeVersion&&report.probeVersion!==data.currentProbeVersion)$('report-results').append(node('p','旧版报告：保留当时的结果与证据，不参与当前版本的音源汇总。请重新检测获取新版结果。','warning'));if(report.error)$('report-results').append(node('p',report.error,'error'));
      const visibleResults=report.results.filter(x=>!path||x.scriptPath===path);if(visibleResults.length)$('report-results').append(reportSummary(visibleResults));
      for(const result of visibleResults){const sample=report.samples.find(x=>x.id===result.sampleId),card=node('details',null,'result-card');card.open=true;card.append(node('summary',`${sample?.title||result.sampleId} · ${platformName(result.platform)} · ${result.scriptPath}`));const meta=node('div',null,'result-meta');
        const init=({ok:'初始化成功',failed:'初始化失败',unsupported:'不支持',not_run:'未初始化'})[result.initialization]||result.initialization;
        const resolve=({ok:'已取得地址',failed:'解析失败',timeout:'解析超时',canceled:'已取消',not_run:'未解析'})[result.resolution]||result.resolution;
        const resource=({not_run:'资源未执行',recognized_audio:'资源结构可识别',invalid:'非音频资源',unreachable:'资源访问失败',unverified:'资源未验证'})[result.resource.status]||result.resource.status;
        if(result.outcome)meta.append(node('span',outcomeName(result.outcome),`badge ${result.outcome==='failed'?'bad':result.outcome==='suspected_short_resource'?'warn':result.outcome==='recognized_audio'?'good':''}`));
        meta.append(node('span',init,'badge'),node('span',resolve,'badge'),node('span',resource,'badge'));if(result.resource.assessment==='suspected_short_resource'&&!result.outcome)meta.append(node('span','疑似短音频','badge warn'));card.append(meta,node('p',`总耗时 ${result.elapsedMs} ms · 初始化 ${result.loadMs??'—'} ms · 解析 ${result.resolveMs??'—'} ms · 资源 ${result.resource.elapsedMs??'—'} ms`,'muted small'),node('p',`预期时长 ${duration(sample?.expectedDurationMs)} · 资源时长 ${duration(result.resource.durationMs)} · ${durationMethod(result.resource.durationMethod)}`,'muted small'));
        if(result.failureStage)card.append(node('p',`失败阶段：${stageName(result.failureStage)} · 归因：${originName(result.failureOrigin)||'未确定'}`,'small'));else if(result.failureOrigin)card.append(node('p',`检查归因：${originName(result.failureOrigin)}`,'small'));
        for(const issue of result.probeIssues||[])card.append(node('p',`检测限制：${issue}`,'warning small'));
        if(result.resource.status!=='not_run'&&result.resolution==='ok'){const r=result.resource;card.append(node('div',`资源 ${r.host||'—'}${r.finalHost&&r.finalHost!==r.host?' → '+r.finalHost:''} · HTTP ${r.httpStatus||'—'} · ${r.contentType||'类型未记录'} · 已读 ${r.bytes??0} bytes${r.contentLength>0?'／响应 '+r.contentLength+' bytes':''}${r.readLimit?' · 已达采样上限':''}`,'resource-evidence'));card.append(node('div',`格式 ${r.format||'未识别'} · 编码 ${r.codec||'未确定'} · 特征 ${r.signature||'旧版未记录'}${r.contentRange?' · Range '+r.contentRange:''}`,'resource-evidence'));if(r.extension||r.headerHex)card.append(node('div',`资源后缀 ${r.extension||'未记录'}${r.headerHex?' · 文件头 '+r.headerHex:''}`,'resource-evidence'));card.append(node('p',`格式：${r.status==='recognized_audio'?'结构已识别':'未确认'} · 时长：${durationAssessment(r)} · 解码：未执行`,'small'));if(r.duplicateSampleIds?.length){const names=r.duplicateSampleIds.map(id=>report.samples.find(s=>s.id===id)?.title||id);card.append(node('p',`重复资源：与 ${names.join('、')} 返回相同完整文件（SHA256 一致），请核对歌曲内容。`,'warning small'));}if(r.sha256)card.append(node('div','完整资源 SHA256 '+r.sha256,'subtext'));if(r.reasonCode)card.append(node('div',`资源判定：${resourceReasonName(r.reasonCode)}（${r.reasonCode}）`,'subtext'));}
        if(result.reason)card.append(node('pre',result.reason));card.append(node('p',`脚本 ${result.scriptSha256} · 检测器 ${result.probeVersion}`,'subtext'));for(const req of result.http||[]){card.append(node('div',`${req.method||''} ${req.host||''} · HTTP ${req.status??'—'} · ${req.elapsedMs} ms${req.businessMessage?' · '+req.businessMessage:''}${req.error?' · '+req.error:''}`,'http-line'));}$('report-results').append(card);}
      if(!report.results.length)$('report-results').append(node('p','任务正在准备，稍后刷新查看结果。','muted'));
    }catch(e){$('report-results').replaceChildren(node('p',e.message,'error'));}}
  $('login-form').addEventListener('submit',async event=>{event.preventDefault();const submit=event.currentTarget.querySelector('button');submit.disabled=true;$('login-error').textContent='';try{const data=await api('/auth/login',{method:'POST',body:JSON.stringify({account:$('account').value.trim(),password:$('password').value})});token=data.data.token;await openSession(data.data.username);}catch(e){logout();$('login-error').textContent=e.message;}finally{submit.disabled=false;}});
  $('sample-form').addEventListener('submit',event=>{event.preventDefault();action(async()=>{const submit=$('sample-form').querySelector('button');submit.disabled=true;submit.textContent='正在获取详情…';try{const data=await api('/admin/source-checks/samples',{method:'POST',body:JSON.stringify({platform:$('sample-platform').value,trackId:$('track-id').value.trim(),expectedVersion:sampleVersion})});samples=data.samples;sampleVersion=data.version;$('track-id').value='';renderSamples();notice('已添加测试歌曲');await refresh();}finally{submit.disabled=false;submit.textContent='获取详情并添加';}});});
  $('sample-platform').addEventListener('change',()=>{$('track-id').placeholder=$('sample-platform').value==='qq'?'例如 002X3Bu82FiBhb':'例如 186016';});
  $('logout').addEventListener('click',logout);$('refresh').addEventListener('click',()=>action(async()=>{await loadSamples();await refresh();notice('状态已刷新');}));$('search').addEventListener('input',renderSources);$('check-all').addEventListener('click',()=>start());
  $('cancel-task').addEventListener('click',()=>action(async()=>{const id=overview?.activeRunId;if(!id)return;await api('/admin/source-checks/runs/'+encodeURIComponent(id)+'/cancel',{method:'POST'});notice('已请求取消，正在回收检测任务');$('cancel-task').disabled=true;await refresh();}));
  $('task-detail').addEventListener('click',()=>{if(overview?.activeRunId)showReport(overview.activeRunId);});$('close-report').addEventListener('click',()=>{$('report-dialog').close();selectedReport='';});$('refresh-report').addEventListener('click',()=>{if(selectedReport)showReport(selectedReport,selectedPath);});
  function tab(name){for(const item of ['sources','samples']){const selected=item===name;$('tab-'+item).setAttribute('aria-selected',String(selected));$('tab-'+item).tabIndex=selected?0:-1;$(item+'-panel').hidden=!selected;}}
  for(const name of ['sources','samples']){$('tab-'+name).addEventListener('click',()=>tab(name));$('tab-'+name).addEventListener('keydown',event=>{if(event.key==='ArrowLeft'||event.key==='ArrowRight'){event.preventDefault();const other=name==='sources'?'samples':'sources';tab(other);$('tab-'+other).focus();}});}
  document.addEventListener('visibilitychange',()=>{if(!document.hidden&&token)refresh();});
  restoreSession();
})();
