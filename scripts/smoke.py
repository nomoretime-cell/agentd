#!/usr/bin/env python3
"""Exercise the built binary with the actual local model. All writes stay local."""
import datetime
import json
import os
import pathlib
import signal
import socket
import subprocess
import time
import urllib.error
import urllib.request

PROJECT = pathlib.Path(__file__).resolve().parent.parent
ROOT = PROJECT / '.cache' / 'binary-e2e' / datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%f')
ROOT.mkdir(parents=True)
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    port = sock.getsockname()[1]
config = (PROJECT / 'config.yaml').read_text()
config = config.replace('127.0.0.1:8090', '127.0.0.1:' + str(port))
config = config.replace('start: "23:00"', 'start: ""').replace('end: "08:00"', 'end: ""')
config_path = ROOT / 'config.yaml'
config_path.write_text(config)
base = 'http://127.0.0.1:' + str(port)
token = ''

def request(path, data=None):
    req = urllib.request.Request(base + path, data=None if data is None else json.dumps(data).encode(),
                                 headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
    return json.load(urllib.request.urlopen(req, timeout=10))

def wait_until(predicate, seconds=120):
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        try:
            if predicate():
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.2)
    raise RuntimeError('Timeout; inspect ' + str(ROOT / 'server.log'))

log = open(ROOT / 'server.log', 'ab')
process = None

def start():
    global process, token
    process = subprocess.Popen([str(PROJECT / 'bin/agentd'), '-root', str(ROOT), '-config', str(config_path)],
                               cwd=str(PROJECT), stdout=log, stderr=log)
    wait_until(lambda: request('/healthz').get('status') == 'ok', 15)
    token = (ROOT / 'data/api-token').read_text().strip()

def stop():
    global process
    if process is not None:
        process.send_signal(signal.SIGTERM)
        process.wait(timeout=30)
        if process.returncode != 0:
            raise RuntimeError('Unexpected exit code: ' + str(process.returncode))
        process = None

try:
    start()
    event = {'key': 'binary-live', 'text': 'Call send_message exactly once with content BINARY_E2E_OK, then finish.'}
    first = request('/v1/events', event)
    second = request('/v1/events', event)
    assert first['id'] == second['id'] and first['inserted'] and not second['inserted']
    wait_until(lambda: any(x['status'] == 'sent' and 'BINARY_E2E_OK' in x['content'] for x in request('/v1/outbox')))
    request('/v1/control', {'command': 'pause'})
    stop()
    start()
    assert request('/v1/status')['paused'] is True
    before = request('/v1/outbox')
    request('/v1/control', {'command': 'resume'})
    time.sleep(1)
    after = request('/v1/outbox')
    assert len(before) == len(after)
    assert len(list((ROOT / 'data/deliveries').glob('*.json'))) == 1
    report = {'passed': True, 'root': str(ROOT), 'listen': base,
              'checks': ['binary startup', 'real model tools', 'input deduplication',
                         'local delivery', 'graceful shutdown', 'pause survives restart', 'no resend after restart']}
    (ROOT / 'report.json').write_text(json.dumps(report, indent=2))
    print(json.dumps(report, indent=2))
finally:
    if process is not None:
        process.terminate()
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
    log.close()
