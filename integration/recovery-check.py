"""Crash/full-storage validation; only run in the disposable test container."""
import errno
import json
import os
import subprocess
import time
import urllib.error
import urllib.request


def command(*args):
    subprocess.run(args, check=True)


def call(path, data=None):
    request = urllib.request.Request(
        "http://127.0.0.1:19100" + path,
        json.dumps(data).encode() if data is not None else None,
        {"Authorization": "Bearer integration-only-token", "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=3) as response:
        return json.load(response)


def ready():
    for attempt in range(100):
        try:
            if call("/v1/health")["recovery_ok"]:
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.2)
    raise AssertionError("agent did not recover")


ready()
request = {"request_id": "crash-" + str(time.time_ns())}
task = call("/v1/tasks", request)
time.sleep(1)
path = "/var/lib/deitysight/tasks/" + task["task_id"]
with open(path + "/samples.jsonl", "ab") as target:
    target.write(b"incomplete-crash-tail")
with open(path + "/result.tar.gz", "wb") as target:
    target.write(b"uncommitted-candidate-must-not-be-served")
command("systemctl", "kill", "--signal=KILL", "--kill-whom=main", "deitysight")
time.sleep(0.5)
ready()
recovered = call("/v1/tasks/" + task["task_id"])
assert recovered["state"] == "interrupted" and recovered["result"]["available"], recovered
assert call("/v1/tasks", request)["reused"]
print("crash recovery:", json.dumps(recovered))

command("systemctl", "stop", "deitysight")
command("mount", "-t", "tmpfs", "-o", "size=8m", "tmpfs", "/var/lib/deitysight")
try:
    command("systemctl", "start", "deitysight")
    ready()
    task = call("/v1/tasks", {"request_id": "disk-full", "window_seconds": 3, "step_seconds": 1})
    time.sleep(0.4)
    with open("/var/lib/deitysight/test-only-fill", "wb", buffering=0) as filler:
        try:
            while True:
                filler.write(b"x" * 65536)
        except OSError as error:
            assert error.errno == errno.ENOSPC, error
    time.sleep(1.3)
    health = call("/v1/health")
    failed = call("/v1/tasks/" + task["task_id"])
    print("full storage:", json.dumps({"health": health, "task": failed}))
    assert failed["state"] == "failed" and not failed["result"]["available"], failed
    assert not health["storage_available"], health
    os.unlink("/var/lib/deitysight/test-only-fill")
    resumed = call("/v1/tasks", {"request_id": "storage-restored", "window_seconds": 1, "step_seconds": 1})
    assert resumed["state"] == "running", resumed
    print("new task accepted after free storage became available")
finally:
    command("systemctl", "stop", "deitysight")
    command("mount", "-o", "remount,rw", "/var/lib/deitysight")
    command("umount", "/var/lib/deitysight")
    command("systemctl", "start", "deitysight")
