"""Run only in the disposable systemd validation container."""
import gzip
import hashlib
import io
import json
import subprocess
import tarfile
import time
import urllib.request


def call(path, data=None):
    body = json.dumps(data).encode() if data is not None else None
    request = urllib.request.Request(
        "http://127.0.0.1:19100" + path, body,
        {"Authorization": "Bearer integration-only-token", "Content-Type": "application/json"},
    )
    return urllib.request.urlopen(request, timeout=10)


workload = subprocess.Popen(["/src/dist/workload"])
try:
    time.sleep(2)
    with call("/v1/tasks", {"request_id": "load-" + str(time.time_ns()),
                            "window_seconds": 6, "step_seconds": 2}) as response:
        task = json.load(response)
    path = "/v1/tasks/" + task["task_id"]
    for attempt in range(100):
        with call(path) as response:
            task = json.load(response)
        if task["state"] != "running":
            break
        time.sleep(0.2)
    assert task["state"] in ("completed", "partial"), task
    with call(path + "/result") as response:
        archive = response.read()
        assert hashlib.sha256(archive).hexdigest() == response.headers["ETag"].strip('"')
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as tar:
        manifest = json.load(tar.extractfile("manifest.json"))
        for item in manifest["files"]:
            raw = tar.extractfile(item["name"]).read()
            assert len(raw) == item["size"]
            assert hashlib.sha256(raw).hexdigest() == item["sha256"]
        records = [json.loads(line) for line in tar.extractfile("samples.jsonl")]
    rounds = {}
    pids, tids = set(), set()
    for record in records:
        rounds[record["sample_id"]] = rounds.get(record["sample_id"], 0) + 1
        obj = record.get("object", {})
        if obj.get("pid"):
            pids.add(obj["pid"])
        if obj.get("tid"):
            tids.add(obj["tid"])
    print(json.dumps({"task_id": task["task_id"], "state": task["state"],
                      "planned_points": task["planned_points"], "sampled_points": task["sampled_points"],
                      "missed_points": task["missed_points"], "errors": task["errors"],
                      "archive_bytes": len(archive), "records_per_round": list(rounds.values()),
                      "observed_pids": len(pids), "observed_tids": len(tids)}, indent=2))
    subprocess.run(["systemctl", "show", "deitysight", "-p", "MemoryCurrent", "-p", "MemoryPeak",
                    "-p", "CPUUsageNSec", "-p", "CPUQuotaPerSecUSec", "-p", "MemoryMax"], check=True)
finally:
    workload.terminate()
    workload.wait(timeout=10)
    # Child workload processes have their own 90-second expiry even after parent termination.
