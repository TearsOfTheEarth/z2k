#!/bin/sh
# Real file transactions: reject stale edits and invalid replacements without
# losing other writers, commit atomically, and allow revision-guarded undo.
ROOT=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
export ROOT
python3 - <<'PY'
import os,pathlib,subprocess,tempfile,concurrent.futures
root=pathlib.Path(os.environ['ROOT'])
with tempfile.TemporaryDirectory() as td:
 p=pathlib.Path(td)/'whitelist.txt'
 env=dict(os.environ,WHITELIST_FILE=str(p),LISTS_DIR=td)
 def call(command,body=''):
  return subprocess.run(['sh','-c','. "$1"; '+command,'test',str(root/'webpanel/cgi/actions.sh')],input=body,text=True,capture_output=True,env=env)
 assert call('command -v whitelist_save').returncode==0, 'bulk save handler is missing'
 def rev():
  r=call('whitelist_revision');assert r.returncode==0,r.stderr;return r.stdout.strip()
 def save(r,body):return call('whitelist_save "$0"'.replace('"$0"',"'"+r+"'"),body)
 p.write_text('# keep comment\na.example\nb.example\n')
 old=p.read_text();r0=rev()
 r=save(r0,'# keep comment\n B.EXAMPLE \r\nb.example\nc.example\n')
 assert r.returncode==0,r.stderr
 assert p.read_text()=='# keep comment\nb.example\nc.example\n'
 r1=rev();assert r1!=r0
 assert save(r0,'lost.example\n').returncode==3,'stale writer must conflict'
 assert p.read_text()=='# keep comment\nb.example\nc.example\n'
 for invalid in ['good.example\nbad domain\n','-bad.example\n','192.168.1.1\n','a..example\n','a;touch /tmp/oops\n']:
  before=p.read_bytes();assert save(r1,invalid).returncode==2,invalid;assert p.read_bytes()==before
 assert save(r1,old).returncode==0,'undo accepted only for current revision'
 assert p.read_text()==old
 r2=rev();assert save(r2,'').returncode==0;assert p.read_text()==''
 assert save(rev(),old).returncode==0
 r3=rev()
 with concurrent.futures.ThreadPoolExecutor(2) as ex:
  results=list(ex.map(lambda body:save(r3,body).returncode,['first.example\n','second.example\n']))
 assert sorted(results)==[0,3],results
 assert p.read_text() in ['first.example\n','second.example\n']
 assert p.stat().st_mode & 0o777 == 0o644
 before=p.read_bytes();r4=rev()
 assert save(r4,'x'*1048577).returncode==2;assert p.read_bytes()==before
 assert not list(pathlib.Path(td).glob('*.z2k-lock')),'lock leaked'
 print('PASS: atomic bulk save, normalization, validation, clear, undo, concurrent conflict, limits and permissions')
PY
