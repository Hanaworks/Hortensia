import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { execFileSync } from 'node:child_process';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
// A linked worktree still uses the original checkout's sibling reference
// projects. Resolve only missing relative links outside this repository there.
let sourceRoot = root;
try {
  const common = execFileSync('git',['rev-parse','--path-format=absolute','--git-common-dir'],{cwd:root,encoding:'utf8',stdio:['ignore','pipe','ignore']}).trim();
  sourceRoot = path.dirname(common);
} catch { /* A source archive can still check links available beside itself. */ }
const skip = new Set(['conversation-notes.md', 'ChatGPT-Docker管理程序推荐-20261030-0119.md']);
const walk = dir => fs.readdirSync(dir, {withFileTypes:true}).flatMap(e =>
  e.isDirectory() ? walk(path.join(dir,e.name)) : e.name.endsWith('.md') && !skip.has(e.name) ? [path.join(dir,e.name)] : []);
const files = [path.join(root,'README.md'), ...walk(path.join(root,'docs'))];
const errors = [];
let links = 0, tables = 0;
const anchor = heading => heading.toLowerCase().replace(/`/g,'').replace(/<[^>]*>/g,'').replace(/[^\p{L}\p{N}\p{M}\s_-]/gu,'').replace(/\s/g,'-');
const headings = file => [...fs.readFileSync(file,'utf8').matchAll(/^#{1,6}\s+(.+)$/gm)].map(m => anchor(m[1].trim()));

for (const file of files) {
  const text = fs.readFileSync(file,'utf8');
  let fenced = false, tableWidth = null;
  for (const [index,line] of text.split(/\r?\n/).entries()) {
    if (/^\s*```/.test(line)) { fenced = !fenced; tableWidth = null; continue; }
    if (fenced) continue;
    if (/^\s*\|/.test(line)) {
      const stripped = line.replace(/`[^`]*`/g,'CODE').replace(/\\\|/g,'PIPE');
      const width = stripped.split('|').length;
      if (tableWidth === null) { tableWidth = width; tables++; }
      else if (tableWidth !== width) errors.push(`${path.relative(root,file)}:${index+1}: table column count differs`);
    } else tableWidth = null;
  }
  if (fenced) errors.push(`${path.relative(root,file)}: unclosed code fence`);
  const prose = text.replace(/```[^\n]*\n[\s\S]*?```/g,'');
  for (const m of prose.matchAll(/\[[^\]\n]+\]\((<[^>]+>|[^)\n]+)\)/g)) {
    const target = m[1].replace(/^<|>$/g,'');
    if (/^(https?:|mailto:)/i.test(target)) continue;
    const [filePart,fragment] = target.split('#');
    let resolved = filePart ? path.resolve(path.dirname(file),decodeURIComponent(filePart)) : file;
    const relative = path.relative(root,resolved);
    if (!fs.existsSync(resolved) && filePart && !path.isAbsolute(filePart) && relative.startsWith('..'+path.sep) && sourceRoot !== root) {
      resolved = path.resolve(sourceRoot,relative);
    }
    links++;
    if (!fs.existsSync(resolved)) { errors.push(`${path.relative(root,file)}: missing ${target}`); continue; }
    if (fragment && resolved.endsWith('.md') && !headings(resolved).includes(decodeURIComponent(fragment).toLowerCase())) {
      errors.push(`${path.relative(root,file)}: missing anchor ${target}`);
    }
  }
}
if (errors.length) { console.error(errors.join('\n')); process.exitCode=1; }
else console.log(JSON.stringify({documents:files.length,localLinks:links,tables,result:'passed (links, anchors, table shapes and fences only)'},null,2));
