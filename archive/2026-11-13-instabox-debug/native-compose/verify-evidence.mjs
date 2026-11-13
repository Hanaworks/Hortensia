import fs from 'node:fs';
import crypto from 'node:crypto';
import path from 'node:path';
const base=path.dirname(new URL(import.meta.url).pathname.replace(/^\/([A-Za-z]:)/,'$1'));
const read=name=>JSON.parse(fs.readFileSync(path.join(base,name),'utf8').replace(/^\uFEFF/,''));
const final=read('final-inventory.json');
const map={'run-native.py':'run-native.py','run-addendum.py':'run-addendum.py','run-20261113-nc1/results.json':'results.json','run-20261113-nc1/commands.jsonl':'commands.jsonl','run-20261113-nc2/results.json':'addendum-results.json','run-20261113-nc2/commands.jsonl':'addendum-commands.jsonl'};
const checks={remote_local_sha256:{},all_resource_kinds_empty:final.all_empty,command_counts:{},case_statuses:{},invalid_original_noimage_excluded:true};
for(const [remote,local] of Object.entries(map)){
  const digest=crypto.createHash('sha256').update(fs.readFileSync(path.join(base,local))).digest('hex');
  checks.remote_local_sha256[local]={sha256:digest,match:digest===final.sha256[remote]};
  if(digest!==final.sha256[remote]) throw Error('mismatched evidence '+local);
}
for(const file of ['commands.jsonl','addendum-commands.jsonl']){
  const commands=fs.readFileSync(path.join(base,file),'utf8').trim().split('\n').map(JSON.parse);
  checks.command_counts[file]=commands.length;
  if(commands.some(x=>x.stdout?.includes('dmnc_fixture_')||x.stderr?.includes('dmnc_fixture_'))) throw Error('fixture secret not redacted');
}
for(const file of ['results.json','addendum-results.json']){
  const d=read(file);
  checks.case_statuses[file]=d.cases.map(({id,status})=>({id,status}));
  if(d.cases.some(x=>x.status!=='PASS')) throw Error('failed recorded case');
}
const old=fs.readFileSync(path.join(base,'commands.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
const corrected=read('addendum-results.json').cases.find(x=>x.id==='NC05b');
if(!old.some(x=>x.stderr?.includes('services.app.command.0 must be a string'))) throw Error('missing original invalid fixture evidence');
if(!corrected.evidence.config_succeeded||!corrected.evidence.up_stderr.includes('No such image')) throw Error('correction missing actual no-image reason');
if(!final.all_empty||Object.values(final.remaining).some(x=>x.length)) throw Error('resources remain');
fs.writeFileSync(path.join(base,'evidence-verification.json'),JSON.stringify(checks,null,2)+'\n');
console.log(JSON.stringify({integrity:'all six source/log hashes match remote',redaction:'no raw fake-token prefix in command outputs',records:checks.command_counts,cleanup:'all three resource kinds empty',NC05:'original invalid YAML excluded; corrected config succeeds then No such image'}));
