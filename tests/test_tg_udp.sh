#!/bin/sh
# Exercise routing installation, repeat repair, rollback and cleanup with
# stateful netfilter/ip stubs; the live router test is recorded in docs.
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
export TMP
cat > "$TMP/mock.py" <<'PY'
import json,os,sys
from pathlib import Path
p=Path(os.environ['TMP'])/'state.json'
s=json.loads(p.read_text()) if p.exists() else {'rules':[],'chains':[],'routes':[],'policy':[]}
a=sys.argv[1:];kind=a.pop(0);rc=0
if kind=='ip':
 fam=a.pop(0) if a[0] in ('-4','-6') else ''
 if a[:2]==['link','show'] or a[:2]==['link','set']:pass
 elif a[:2]==['route','replace']:
  if fam=='-6' and os.environ.get('FAIL6')=='1':rc=1
  elif fam not in s['routes']:s['routes'].append(fam)
 elif a[:2]==['route','flush']:s['routes']=[v for v in s['routes'] if v!=fam]
 elif a[:2]==['rule','show']:
  if fam in s['policy']:print('89: from all fwmark 0x8000000/0x8000000 lookup 988')
 elif a[:2]==['rule','add']:s['policy'].append(fam)
 elif a[:2]==['rule','del']:
  if fam in s['policy']:s['policy'].remove(fam)
  else:rc=1
 else:raise RuntimeError(a)
else:
 tab='filter'
 if a[0]=='-t':tab=a[1];a=a[2:]
 op,chain=a[:2];rule=a[2:]
 if op=='-I':rule=rule[1:]
 key=[kind,tab,chain,rule];ck=[kind,tab,chain]
 if op=='-C':rc=0 if key in s['rules'] else 1
 elif op in ('-I','-A'):s['rules'].append(key)
 elif op=='-D':s['rules'].remove(key)
 elif op=='-N':
  if ck in s['chains']:rc=1
  else:s['chains'].append(ck)
 elif op=='-F':s['rules']=[r for r in s['rules'] if r[:3]!=ck]
 elif op=='-X':
  if ck in s['chains']:s['chains'].remove(ck)
 else:raise RuntimeError(a)
p.write_text(json.dumps(s));sys.exit(rc)
PY
. "$ROOT/files/z2k-tg-redirect.sh"
# shellcheck disable=SC2034 # consumed by the sourced helper
Z2K_TG_UDP_IF=lab0
Z2K_TG_UDP_READY="$TMP/ready"
ip() { python3 "$TMP/mock.py" ip "$@"; }
ipset() { :; }
_z2k_tg_ipt() { python3 "$TMP/mock.py" 4 "$@"; }
_z2k_tg_ipt6() { python3 "$TMP/mock.py" 6 "$@"; }
z2k_tg_udp_ensure
[ ! -e "$TMP/state.json" ] # no authenticated capability => no routing
: > "$Z2K_TG_UDP_READY"
z2k_tg_udp_ensure
cp "$TMP/state.json" "$TMP/first.json"
z2k_tg_udp_ensure
cmp "$TMP/state.json" "$TMP/first.json"
python3 - <<'PY'
import json,os
s=json.load(open(os.environ['TMP']+'/state.json'))
assert sorted(s['policy'])==['-4','-6']
for family in ['4','6']:
 rules=[r for r in s['rules'] if r[0]==family]
 divert=[r for r in rules if r[2]=='PREROUTING' and r[3][-1]=='Z2K_TG_UDP']
 assert len(divert)==1
 assert divert[0][3][:4]==['-i','br+','-p','udp']
 assert '--match-set' in divert[0][3] and 'dst' in divert[0][3]
 assert not any(r[2]=='OUTPUT' for r in rules)
 assert any(r[1:3]==['nat','POSTROUTING'] for r in rules)
PY
z2k_tg_udp_down
python3 - <<'PY'
import json,os
s=json.load(open(os.environ['TMP']+'/state.json'));assert all(not v for v in s.values()),s
PY
: > "$Z2K_TG_UDP_READY"
export FAIL6=1
if z2k_tg_udp_ensure; then echo 'FAIL: IPv6 failure silently accepted'; exit 1; fi
python3 - <<'PY'
import json,os
s=json.load(open(os.environ['TMP']+'/state.json'));assert all(not v for v in s.values()),s
PY
[ ! -e "$Z2K_TG_UDP_READY" ]
echo 'PASS: UDP capability gate, IPv4/IPv6 routes, idempotence, LAN-only scope, cleanup, rollback'

for flag in 'Z2K_TG_UDP_RELAY=1' 'Z2K_TG_UDP_RELAY="1"'; do
    printf '%s\n' "$flag" > "$TMP/config"
    z2k_tg_udp_enabled "$TMP/config"
done
printf 'Z2K_TG_UDP_RELAY=0\n' > "$TMP/config"
if z2k_tg_udp_enabled "$TMP/config"; then exit 1; fi
z2k_tg_udp_enabled "$TMP/missing"
printf 'TG_PROXY_USER_DISABLED=0\n' > "$TMP/config"
z2k_tg_udp_enabled "$TMP/config"
