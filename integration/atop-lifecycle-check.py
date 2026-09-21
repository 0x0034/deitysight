#!/usr/bin/env python3
"""Lifecycle regression; root on the explicitly authorized R9 test VM only."""
import hashlib,io,json,subprocess,tarfile,time,urllib.request,urllib.error
from pathlib import Path
BASE=Path('/opt/deitysight-regression')
config=BASE/'agent.yaml'
original=config.read_text()
headers={'Authorization':'Bearer '+(BASE/'token').read_text().strip(),'Content-Type':'application/json'}
def call(path,data=None,status=200):
 req=urllib.request.Request('http://127.0.0.1:19100'+path,headers=headers,data=None if data is None else json.dumps(data).encode())
 try:res=urllib.request.urlopen(req,timeout=10)
 except urllib.error.HTTPError as e:res=e
 with res:
  b=res.read();assert res.status==status,(res.status,b)
  return b

def restart(text):
 subprocess.run(['systemctl','stop','deitysight-regression'],check=True)
 config.write_text(text)
 subprocess.run(['systemctl','start','deitysight-regression'],check=True)
 deadline=time.monotonic()+10
 while time.monotonic()<deadline:
  try:
   urllib.request.urlopen('http://127.0.0.1:19100/v1/health',timeout=1)
  except urllib.error.HTTPError:return
  except OSError:time.sleep(.1)
 raise RuntimeError('HTTP did not start')

def task(window,threads=False):
 return json.loads(call('/v1/tasks',{'request_id':'lifecycle-'+str(time.time_ns()),'window_seconds':window,'step_seconds':1,'include_threads':threads},202))

def terminal(t):
 deadline=time.monotonic()+25
 while t['state']=='running' and time.monotonic()<deadline:
  time.sleep(.2);t=json.loads(call('/v1/tasks/'+t['task_id']))
 assert t['state']!='running',t
 return t

def records(t,name='samples.jsonl'):
 b=call(t['result']['url']);assert hashlib.sha256(b).hexdigest()==t['result']['sha256']
 tf=tarfile.open(fileobj=io.BytesIO(b))
 return [json.loads(x) for x in tf.extractfile(name)]

try:
 restart(original+'background:\n  enabled: true\n  step: 1s\n  retention: 10s\n')
 time.sleep(3)
 t=terminal(task(2,True));assert t['state']=='completed' and t['history_records']>0,t
 r=records(t);assert any(x.get('scope')=='thread' for x in r)
 assert all(x['schema_version']==2 for x in records(t,'history.jsonl'))
 print('background preemption, retained history and explicit threads: PASS',flush=True)
 t=task(20);time.sleep(2.5)
 restart(original)
 t=json.loads(call('/v1/tasks/'+t['task_id']))
 assert t['state']=='interrupted' and t['sampled_points']>=1,t
 r=records(t);assert sum(x['kind']=='frame_end' for x in r)==t['sampled_points']
 old_url=t['result']['url'];old_hash=t['result']['sha256']
 print('restart packages existing committed frames: PASS',flush=True)
 restart(original.replace('enabled: true','enabled: false'))
 call('/v1/health',status=503)
 call('/v1/tasks',{'request_id':'unavailable-'+str(time.time_ns())},503)
 assert hashlib.sha256(call(old_url)).hexdigest()==old_hash
 print('unavailable atop keeps HTTP and old downloads, rejects new tasks: PASS',flush=True)
finally:
 restart(original)
