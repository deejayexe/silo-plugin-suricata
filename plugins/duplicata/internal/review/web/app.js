'use strict';
const $ = id => document.getElementById(id);
const state = {csrf: '', status: 'idle', id: '', offset: 0, total: 0, busy: false, readOnly: true, plan: null, activeId: '', activeStatus: 'idle', selectedId: '', historical: false, view: 'analysis'};
const labels = {idle:'No scan',running:'Scanning',complete:'Complete',partial:'Incomplete',cancelled:'Stopped',failed:'Failed'};
const theme = new URLSearchParams(location.search).get('theme');
if (theme === 'light') document.documentElement.dataset.theme = 'light';
async function api(path, body) {
  const response = await fetch(new URL('./' + path, location.href), {
    method: body ? 'POST' : 'GET',
    credentials: 'same-origin',
    headers: body ? {'Content-Type':'application/json'} : {},
    body: body ? JSON.stringify({...body,csrf:state.csrf}) : undefined
  });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || 'Could not complete the operation');
  return data;
}
function buttons() {
  const running = state.activeStatus === 'running';
  $('start').disabled = state.busy || running || !state.connected || !$('library').value || !state.csrf;
  $('cancel').disabled = state.busy || !running;
  $('library').disabled = running || state.busy;
  $('clear').disabled = state.busy || running || state.historical;
  $('previous').disabled = state.busy || state.offset === 0;
  $('next').disabled = state.busy || state.offset + 20 >= state.total;
}
async function act(fn) {
  state.busy = true; $('error').textContent = ''; $('operation-message').textContent=''; buttons();
  try { await fn(); } catch (e) { $('error').textContent = e.message; }
  finally { state.busy = false; buttons(); }
}
function node(tag, text, className) {
  const el = document.createElement(tag);
  if (text !== undefined) el.textContent = text;
  if (className) el.className = className;
  return el;
}
const gib = n => ((n || 0) / 1024 ** 3).toLocaleString('en', {maximumFractionDigits:2}) + ' GiB';
const capacity = n => {n=Math.max(0,n||0);const units=['B','KiB','MiB','GiB','TiB','PiB'];let unit=0;while(n>=1024&&unit<units.length-1){n/=1024;unit++;}return n.toLocaleString('en',{maximumFractionDigits:2})+' '+units[unit];};
const spaceText = estimate => estimate ? 'Estimated space released: '+capacity(estimate.bytes)+(estimate.basis==='hardlinks'?' · Other hardlinks retain the data':'') : 'Space released: unknown';
const length = seconds => seconds ? Math.floor(seconds/3600)+' h '+Math.floor(seconds%3600/60)+' min' : '—';
function render(groups) {
  const container = $('groups'); container.replaceChildren();
  if (!groups?.length) {
    container.append(node('p', state.status==='complete' ? 'No duplicate groups found in this library.' : 'Results appear when the scan finishes or stops.', 'empty'));
    return;
  }
  for (const group of groups) {
    const card=node('article',undefined,'group');
    const heading=node('div',undefined,'group-heading');
    const poster=node('img',undefined,'poster');poster.alt='';poster.loading='lazy';poster.src=new URL('./poster?group='+encodeURIComponent(group.id)+'&scan_id='+encodeURIComponent(state.id),location.href);poster.onerror=()=>{poster.replaceWith(node('span','▧','poster placeholder'));};heading.append(poster);
    heading.append(node('h3', (group.series_title ? group.series_title+' · ' : '') + group.title + (group.year ? ' ('+group.year+')' : '')),
      node('span',group.versions.length+' versions','badge'));
    card.append(heading);
    const wrap=node('div',undefined,'table-scroll'), table=node('table');
    const head=node('thead'), hr=node('tr');
    for (const title of ['File / edition','Size','Video','Duration','Audio','Action']) hr.append(node('th',title));
    head.append(hr); table.append(head);
    const tbody=node('tbody');
    for (const v of group.versions) {
      const row=node('tr'), file=node('td');file.className='file-cell';
      file.append(node('strong',v.file_path.split('/').pop()),node('span',v.edition_raw || 'Unidentified edition','tag'));
      const paths=node('details',undefined,'file-paths');paths.open=matchMedia('(min-width:851px)').matches;paths.append(node('summary','File paths'),node('small',v.file_path));
      if(v.resolved_path&&v.resolved_path!==v.file_path)paths.append(node('small','Resolved: '+v.resolved_path));
      if(v.kept_path)paths.append(node('small','Retained: '+v.kept_path));
      for(const path of v.removed_directories||[])paths.append(node('small','Removed empty folder: '+path));
      file.append(paths);
      const video=node('td');video.append(node('span',v.resolution||'—'));
      const codec=(v.codec_video||'').toLowerCase(),family=codec.includes('av1')?'av1':/hevc|265/.test(codec)?'hevc':/264|avc/.test(codec)?'h264':'other';
      video.append(node('span',codec.toUpperCase()||'—','codec '+family));
      row.append(file,node('td',gib(v.file_size)),video,
        node('td',length(v.duration)),node('td',[(v.audio_tracks||[]).map(a=>a.language?.toUpperCase()).filter(Boolean).join(', '),v.codec_audio?.toUpperCase()].filter(Boolean).join(' · ')||'—'));
      for(const [index,cell] of [...row.children].entries())cell.dataset.label=['File / edition','Size','Video','Duration','Audio'][index];
      const action=node('td');action.className='action-cell';action.dataset.label='Action';
      if(v.deleted){row.classList.add('deleted');action.append(node('span','Deleted'+(v.deleted_at?' · '+new Date(v.deleted_at).toLocaleString():''),'deleted-label'),node('small',spaceText(v.space_released)));}
      else if(!state.historical&&!state.readOnly&&state.status==='complete'){
        const button=node('button','Review deletion');button.disabled=!v.can_delete||!!v.write_blocked||!group.versions.some(x=>!x.deleted&&x.can_delete&&x.resolved_path!==v.resolved_path);
        button.title=v.blocked||v.write_blocked||'Another verified file path must remain';
        button.onclick=()=>act(async()=>{
          const request={scan_id:state.id,group_id:group.id,file_id:v.file_id};
          const preview=await api('delete/preview',request);state.plan={...request,token:preview.token};
          $('delete-path').textContent=preview.path+' · '+gib(preview.size);$('keep-path').textContent=preview.keep_path;$('delete-error').textContent='';$('delete-dialog').showModal();
        });action.append(button);

      }else action.append(node('span',state.historical?'History · read only':state.readOnly?'Deletion disabled in settings':'Complete scan required'));
      if(v.blocked||v.write_blocked)action.append(node('small',v.blocked||v.write_blocked));
      row.append(action);
      tbody.append(row);
    }
    table.append(tbody);wrap.append(table);card.append(wrap);
    for(const warning of group.warnings||[]) card.append(node('p',warning,'warning'));
    container.append(card);
  }
}
function scrollToResults() {
  // Wait for the newly visible results to be laid out before replacing the
  // browser's scroll position retained from history or the previous page.
  requestAnimationFrame(()=>{
    $('groups').scrollIntoView({block:'start',behavior:'instant'});
  });
}
async function results() {
  const data=await api('results?offset='+state.offset+'&scan_id='+encodeURIComponent(state.selectedId));
  state.id=data.scan_id;state.status=data.status;state.historical=data.historical;
  $('viewing').textContent=state.selectedId?(data.library_name||'Analysis')+' · '+(labels[data.status]||data.status)+(data.historical?' · Saved results (read only)':' · Current analysis'):'';
  $('back-current').hidden=!state.selectedId;
  if(state.selectedId)$('error').textContent=(data.errors||[]).join('\n');
  state.total=data.total;
  const space=data.space||{deleted_files:0};$('reclaimed').hidden=!space.deleted_files;
  $('reclaimed').textContent=space.deleted_files+' deleted files · Estimated space released: '+capacity(space.estimated_bytes)+(space.unknown_files?' · '+space.unknown_files+' unknown':'');
  $('reclaimed').title='Estimate from allocated blocks. Hardlinks release no file data; open files, shared blocks and snapshots can delay or prevent actual recovery.';
  $('count').textContent='Groups to review · '+state.total;
  $('page').textContent=state.total ? (state.offset+1)+'–'+Math.min(state.offset+20,state.total)+' of '+state.total : '';
  render(data.groups);buttons();
}
async function poll() {
  const data=await api('status');
  const changed=state.activeStatus!==data.status || state.activeId!==data.id;
  if(state.activeId!==data.id && !state.selectedId) state.offset=0;
  state.readOnly=data.read_only;$('mode').textContent=(state.readOnly?'Deletion disabled':'Deletion enabled');
  state.csrf=data.csrf;state.activeStatus=data.status;state.activeId=data.id;
  $('status').textContent=(labels[data.status]||data.status)+' · '+data.processed+' / '+data.total+' titles';
  $('progress').max=data.total||1;$('progress').value=data.processed;
  if(data.errors?.length) $('error').textContent=data.errors.join('\n');
  if(changed || (!state.id&&!state.selectedId)) await results();
  if(state.view==='history' && (changed || data.status==='running')) await history();
  buttons();
}
function connectionStatus(status, label) {
  $('connection-state').textContent=label;
  $('connection-icon').dataset.state=status;
  state.connected=status==='connected';
  buttons();
}
async function connect() {
  let data;
  try {data=await api('libraries');} catch(e) {const option=node('option','Could not connect');option.value='';$('library').replaceChildren(option);$('connection').textContent=e.message;connectionStatus('disconnected','Disconnected');throw e;}
  connectionStatus('connected','Connected');
  const current=$('library').value;$('library').replaceChildren();
  for(const lib of data.items||[]) {const option=node('option',lib.name);option.value=lib.id;$('library').append(option);}
  if([...$('library').options].some(o=>o.value===current)) $('library').value=current;
  $('connection').textContent='Profile '+data.profile_name+' · '+data.items.length+' libraries';
  buttons();
}
$('library').addEventListener('change',buttons);
$('start').onclick=()=>act(async()=>{await api('scan',{library_id:$('library').value});state.offset=0;state.selectedId='';await poll();});
$('cancel').onclick=()=>act(async()=>{await api('cancel',{});await poll();});
$('clear').onclick=()=>act(async()=>{await api('clear',{});state.offset=0;state.selectedId='';await poll();});
$('previous').onclick=()=>act(async()=>{state.offset=Math.max(0,state.offset-20);await results();scrollToResults();});
$('next').onclick=()=>act(async()=>{state.offset+=20;await results();scrollToResults();});
let lastConnectionCheck=Date.now();
async function tick() {
  try {if(!state.busy) {await poll();if(Date.now()-lastConnectionCheck>=30000){lastConnectionCheck=Date.now();await connect();}}}
  catch(e) {$('error').textContent=e.message;}
  finally {setTimeout(tick,1500);}
}
act(async()=>{await poll();await connect();}).finally(()=>setTimeout(tick,1500));

$('dismiss-delete').onclick=()=>{$('delete-dialog').close();state.plan=null;};
$('confirm-delete').onclick=async()=>{
 if(!state.plan)return;
 $('confirm-delete').disabled=true;$('dismiss-delete').disabled=true;
 try{const result=await api('delete/confirm',state.plan);state.plan=null;$('delete-dialog').close();$('operation-message').textContent=result.message+' '+spaceText(result.space_released)+'.';await results();}
 catch(e){$('delete-error').textContent=e.message;state.plan=null;}
 finally{$('confirm-delete').disabled=false;$('dismiss-delete').disabled=false;}
};

function showView(view) {
  state.view=view;
  $('analysis-panel').hidden=view!=='analysis';$('history-panel').hidden=view!=='history';
  $('analysis-tab').setAttribute('aria-pressed',String(view==='analysis'));
  $('history-tab').setAttribute('aria-pressed',String(view==='history'));
}
async function history() {
  const data=await api('history');const list=$('history-list');list.replaceChildren();
  if(!data.items.length){list.append(node('p','No analyses in memory yet.','empty'));return;}
  for(const entry of data.items){
    const card=node('article',undefined,'history-entry'),info=node('div');
    info.append(node('h3',entry.library_name||'Library'),node('p',(entry.started_at&& !entry.started_at.startsWith('0001')?new Date(entry.started_at).toLocaleString()+' · ':'')+(labels[entry.status]||entry.status)+(entry.current?' · Current':'')+(entry.finished_at&&!entry.finished_at.startsWith('0001')?' · Finished '+new Date(entry.finished_at).toLocaleString():'' )),node('p',entry.processed+' / '+entry.total+' titles · '+entry.group_count+' groups · '+entry.deleted_count+' deleted files'+(entry.deleted_count?' · Estimated space released: '+capacity(entry.space?.estimated_bytes)+(entry.space?.unknown_files?' · '+entry.space.unknown_files+' unknown':''):'')));
    const actions=node('div',undefined,'history-actions'),open=node('button',entry.current?'View results':'Continue review');
    open.disabled=!entry.current&&state.activeStatus==='running';
    open.onclick=()=>act(async()=>{
      if(!entry.current)await api('history/restore',{scan_id:entry.id});
      state.selectedId='';state.offset=0;state.plan=null;await poll();await results();showView('analysis');
      $('operation-message').textContent=entry.status==='complete'?'Analysis ready for review. Files are checked again before deletion.':'Saved results opened. A complete scan is required for deletion.';
      scrollToResults();
    });actions.append(open);
    if(!entry.current){const remove=node('button','Remove record');remove.onclick=()=>act(async()=>{await api('history/remove',{scan_id:entry.id});if(state.selectedId===entry.id){state.selectedId='';state.offset=0;await results();}await history();});actions.append(remove);}
    card.append(info,actions);list.append(card);
  }
}
$('analysis-tab').onclick=()=>showView('analysis');
$('history-tab').onclick=()=>act(async()=>{await history();showView('history');});
$('back-current').onclick=()=>act(async()=>{state.selectedId='';state.offset=0;await results();});

matchMedia('(max-width:850px)').addEventListener('change',event=>{for(const paths of document.querySelectorAll('.file-paths'))paths.open=!event.matches;});
