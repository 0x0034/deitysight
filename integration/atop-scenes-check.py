#!/usr/bin/env python3
"""Bounded scenario regression on an explicitly authorized disposable Linux host.
Requires the regression service, its token file, and atop 2.7.1 already installed.
Only this external verifier computes assertions; the agent never computes metrics.
"""
import collections
import hashlib
import io
import json
import mmap
import os
import socket
import subprocess
import sys
import tarfile
import threading
import time
import urllib.request

BASE = '/opt/deitysight-regression'


def workload(scene):
    end = time.monotonic() + 9
    if scene == 'cpu':
        while time.monotonic() < end:
            sum(i*i for i in range(10000))
    elif scene == 'mem':
        data = bytearray(64 << 20)
        while time.monotonic() < end:
            for i in range(0, len(data), 4096):
                data[i] = (data[i]+1) % 256
            time.sleep(.1)
    elif scene == 'io':
        path = BASE + '/bounded-io.tmp'
        fd = os.open(path, os.O_CREAT | os.O_RDWR | os.O_DIRECT, 0o600)
        data = mmap.mmap(-1, 4 << 20)
        try:
            while time.monotonic() < end:
                os.lseek(fd, 0, 0)
                os.write(fd, data)
                os.fsync(fd)
                os.lseek(fd, 0, 0)
                os.read(fd, 0)  # writes provide process I/O attribution, no cache-only assertion
                time.sleep(.2)
        finally:
            os.close(fd)
            data.close()
            os.unlink(path)
    elif scene == 'network':
        server = socket.socket()
        server.bind(('127.0.0.1', 0))
        server.listen(1)
        def reader():
            conn, _ = server.accept()
            with conn:
                while conn.recv(65536):
                    pass
        thread = threading.Thread(target=reader)
        thread.start()
        with socket.create_connection(server.getsockname()) as conn:
            while time.monotonic() < end:
                conn.sendall(b'x' * 65536)
                time.sleep(.002)
        thread.join()
        server.close()


def check():
    token = open(BASE + '/token').read().strip()
    def request(path, data=None):
        req = urllib.request.Request('http://127.0.0.1:19100'+path,
              data=None if data is None else json.dumps(data).encode(),
              headers={'Authorization':'Bearer '+token,'Content-Type':'application/json'})
        with urllib.request.urlopen(req, timeout=15) as res:
            return res.read()
    report = []
    for scenes in [['cpu'], ['io'], ['mem'], ['network'], ['cpu','io','mem','network']]:
        processes = {s: subprocess.Popen([sys.executable, __file__, '--load', s, 'REDACTION_SENTINEL']) for s in scenes}
        try:
            time.sleep(.3)
            body = {'request_id':'scenes-'+str(time.time_ns()),'window_seconds':4,'step_seconds':1,'scenes':scenes}
            task = json.loads(request('/v1/tasks', body))
            replay = json.loads(request('/v1/tasks', body))
            assert replay['task_id'] == task['task_id']
            deadline = time.monotonic()+25
            while task['state']=='running' and time.monotonic()<deadline:
                time.sleep(.2)
                task = json.loads(request('/v1/tasks/'+task['task_id']))
            assert task['state']=='completed', task
            data = request(task['result']['url'])
            assert hashlib.sha256(data).hexdigest()==task['result']['sha256']
            tf = tarfile.open(fileobj=io.BytesIO(data))
            manifest = json.load(tf.extractfile('manifest.json'))
            assert manifest['schema_version']==2
            lines = tf.extractfile('samples.jsonl').read()
            assert b'REDACTION_SENTINEL' not in lines
            records = [json.loads(x) for x in lines.splitlines()]
            assert sum(r['kind']=='frame_end' for r in records)==5
            assert not any(r['scope']=='thread' for r in records if 'scope' in r)
            intervals = [r for r in records if r['kind']=='source' and not r.get('baseline',False)]
            def values(scene, label):
                return [r['content'].split(') ',1)[1].split() for r in intervals
                        if r['source']=='atop/'+label and r.get('object',{}).get('pid')==processes[scene].pid]
            if 'cpu' in scenes:
                rows = values('cpu','PRC')
                assert rows and sum(int(v[2])+int(v[3]) for v in rows)>0
            if 'io' in scenes:
                rows = values('io','PRD')
                assert rows and all(v[2]=='y' for v in rows) and sum(int(v[6]) for v in rows)>0
            if 'mem' in scenes:
                rows = values('mem','PRM')
                assert rows and max(int(v[3]) for v in rows)>60*1024
            if 'network' in scenes:
                rows = [r['content'].split()[6:] for r in intervals if r['source']=='atop/NET']
                assert any(v[0]=='lo' and int(v[2])>0 for v in rows), rows
                assert task['capabilities'].get('atop/PRN') is False, 'R9 has no netatop plugin'
            name = '-'.join(scenes)
            open(BASE+'/'+name+'.tar.gz','wb').write(data)
            report.append({'scenes':scenes,'state':task['state'],'frames':task['sampled_points'],
                           'source_records':task['source_records'],'capabilities':task['capabilities'],
                           'archive_bytes':len(data),'task_id':task['task_id']})
            print(json.dumps(report[-1]), flush=True)
        finally:
            for p in processes.values():
                if p.poll() is None:
                    p.terminate()
                p.wait(timeout=3)
            path=BASE+'/bounded-io.tmp'
            if os.path.exists(path):
                os.unlink(path)
    open(BASE+'/scenario-report.json','w').write(json.dumps(report,indent=2)+'\n')


if __name__ == '__main__':
    if len(sys.argv)>1 and sys.argv[1]=='--load':
        workload(sys.argv[2])
    else:
        check()
