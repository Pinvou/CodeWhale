"""Replay the actual connector Node contract with 60 derived negative controls.

Uses a temporary fs.readFileSync override in fresh Node processes; repository
workflows and tests remain unchanged. Requires Python 3 and Node.js.
"""
import tempfile
from pathlib import Path
import subprocess,json,re,hashlib,datetime
repo = Path(__file__).resolve().parents[3]
with tempfile.TemporaryDirectory(prefix="pinvou-matrix-controls-") as directory:
    state = Path(directory)
    source=(repo/'.github/workflows/connector-verify.yml').read_text()
    test=repo/'pinvou3-app/tests/tauri_platform_layout.test.js'
    match=re.search(r"(?m)^      matrix: \$\{\{ fromJSON\(vars\.PINVOU_WINDOWS_MAC_ENABLED != 'false' && '([^']+)' \|\| '([^']+)'\) \}\}$",source)
    assert match
    matrixline=match.group(0);enabled=json.loads(match[1]);disabled=json.loads(match[2])
    def new_line(a=enabled,b=disabled):
     return "      matrix: ${{ fromJSON(vars.PINVOU_WINDOWS_MAC_ENABLED != 'false' && '"+json.dumps(a,separators=(',',':'))+"' || '"+json.dumps(b,separators=(',',':'))+"') }}"
    def replace(a=enabled,b=disabled):return source.replace(matrixline,new_line(a,b),1)
    cases=[]
    for branch,base in [('enabled',enabled),('disabled',disabled)]:
     for i,row in enumerate(base['include']):
      for key in ['platform','runs-on','resources','suffix','can_run']:
       changed=json.loads(json.dumps(base));changed['include'][i][key]=False if key=='can_run' else row[key]+'-wrong'
       cases.append((f'{branch}-row{i}-{key}',replace(changed,disabled) if branch=='enabled' else replace(enabled,changed)))
      changed=json.loads(json.dumps(base));changed['include'].pop(i)
      cases.append((f'{branch}-missing-row{i}',replace(changed,disabled) if branch=='enabled' else replace(enabled,changed)))
     changed=json.loads(json.dumps(base));changed['include'].append(changed['include'][0])
     cases.append((f'{branch}-duplicate-row',replace(changed,disabled) if branch=='enabled' else replace(enabled,changed)))
     cases.append((f'{branch}-wrong-shape',replace({'rows':base['include']},disabled) if branch=='enabled' else replace(enabled,{'rows':base['include']})))
     cases.append((f'{branch}-malformed-json',source[:match.start(1 if branch=='enabled' else 2)]+"{"+source[match.end(1 if branch=='enabled' else 2):]))
    cases.extend([
     ('commented-matrix',source.replace(matrixline,'#'+matrixline,1)),
     ('missing-matrix',source.replace(matrixline,'',1)),
     ('duplicate-matrix',source.replace(matrixline,matrixline+'\n'+matrixline,1)),
     ('duplicate-job',source+"\n  verify-connectors:\n    runs-on: ubuntu-22.04\n"),
     ('duplicate-jobs-map',source+"\njobs:\n  decoy:\n    runs-on: ubuntu-22.04\n"),
     ('commented-job',source.replace('  verify-connectors:','#  verify-connectors:',1)),
     ('wrong-job',source.replace('  verify-connectors:','  decoy:',1)),
     ('duplicate-strategy',source.replace('    strategy:','    strategy:\n    strategy:',1)),
     ('matrix-outside-strategy',source.replace(matrixline,'    runs-on: ubuntu-22.04\n'+matrixline,1)),
     ('inverted-condition',source.replace("vars.PINVOU_WINDOWS_MAC_ENABLED != 'false'","vars.PINVOU_WINDOWS_MAC_ENABLED == 'false'",1)),
     ('swapped-branches',replace(disabled,enabled)),
     ('wrong-job-runner',source.replace("    runs-on: ${{ matrix.runs-on == 'ubuntu-22.04'", "    runs-on: ${{ matrix.runs-on == 'wrong-runner'",1)),
    ])
    assert len({label for label,_ in cases})==len(cases)
    assert all(mutated!=source for _,mutated in cases)
    hook=state/'oct29-review569630-layout-workflow-read-hook.cjs'
    fixture=state/'oct29-review569630-layout-mutated-workflow.yml'
    hook.write_text('''const fs = require("node:fs");
    const path = require("node:path");
    const read = fs.readFileSync;
    const target = process.env.PINVOU_LAYOUT_WORKFLOW;
    const replacement = read(process.env.PINVOU_LAYOUT_MUTATION);
    fs.readFileSync = function (file, options) {
      if (typeof file === "string" && path.resolve(file) === target) {
        const encoding = typeof options === "string" ? options : options?.encoding;
        return encoding ? replacement.toString(encoding) : Buffer.from(replacement);
      }
      return read.apply(this, arguments);
    };
    ''')
    import os
    env=dict(os.environ,PINVOU_LAYOUT_WORKFLOW=str(repo/'.github/workflows/connector-verify.yml'),PINVOU_LAYOUT_MUTATION=str(fixture))
    results=[];output=[]
    for label,value,expect in [('positive-before',source,0)]+[(a,b,1) for a,b in cases]+[('positive-after',source,0)]:
     fixture.write_text(value)
     p=subprocess.run(['node','--require',str(hook),str(test)],cwd=repo,env=env,text=True,capture_output=True,timeout=30)
     valid=(p.returncode==0) if expect==0 else (p.returncode!=0 and ('AssertionError' in p.stderr or 'SyntaxError' in p.stderr))
     result={'name':label,'exit':p.returncode,'expected_rejected':bool(expect),'passed':valid,'mutated_workflow_sha256':hashlib.sha256(value.encode()).hexdigest()}
     results.append(result);output.append(f'CASE {label} exit={p.returncode} expected_rejected={bool(expect)} valid={valid}\n'+p.stdout+p.stderr)
     print(f"CASE {label}: exit={p.returncode}, expected_rejected={bool(expect)}, valid={valid}", flush=True)
     if not valid:
      print(p.stdout, p.stderr)
      break
    (state/'oct29-review569630-layout-negative-controls.log').write_text('\n'.join(output))
    report={'at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'actual_entry':'node --require <workflow-read-hook> pinvou3-app/tests/tauri_platform_layout.test.js','method':'Run the actual complete Node test in a fresh process, substituting only fs.readFileSync of the actual connector workflow path with a derived fixture; no workflow/repository mutations or alternate assertion algorithm.','control_cases':len(cases),'executed':len(results),'allpassed':all(x['passed'] for x in results) and len(results)==len(cases)+2,'workflow_sha256_unchanged':hashlib.sha256((repo/'.github/workflows/connector-verify.yml').read_bytes()).hexdigest()==hashlib.sha256(source.encode()).hexdigest(),'test_sha256':hashlib.sha256(test.read_bytes()).hexdigest(),'results':results,'log':'oct29-review569630-layout-negative-controls.log'}
    (state/'oct29-review569630-layout-negative-controls.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
    print(json.dumps({k:v for k,v in report.items() if k!='results'},ensure_ascii=False))
    assert report['allpassed']
