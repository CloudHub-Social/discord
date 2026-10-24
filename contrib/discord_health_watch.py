#!/usr/bin/env python3
"""Run a read-only host probe over verified SSH; deduplicate alert attempts.

Local state records an alert attempt, not confirmation of notification delivery.
"""
import argparse
import datetime
import fcntl
import hashlib
import json
import os
import pathlib
import re
import subprocess
import tempfile


def unknown(reason):
    return {'status': 'unknown', 'needs_attention': True,
            'findings': [{'session': 'monitor', 'reason': reason, 'unknown': True}]}


def run_check(host, expected):
    if not re.fullmatch(r'[A-Za-z0-9_.@:-]+', host) or host.startswith('-') or not 1 <= expected <= 100:
        return unknown('monitor_arguments_invalid')
    remote = 'sudo -n python3 /usr/local/libexec/matrix-discord-health.py --expected-sessions ' + str(expected)
    args = ['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=8', '-o', 'StrictHostKeyChecking=yes',
            '-o', 'ServerAliveInterval=10', '-o', 'ServerAliveCountMax=2', host, remote]
    try:
        proc = subprocess.run(args, capture_output=True, text=True, timeout=90)
        if proc.returncode not in (0, 1, 2):
            return unknown('ssh_unavailable')
        if len(proc.stdout) > 65536:
            return unknown('probe_output_limit')
        result = json.loads(proc.stdout)
        assert result['status'] in ('healthy', 'starting', 'recovering', 'alert', 'unknown')
        assert type(result['needs_attention']) is bool and isinstance(result['findings'], list)
        assert result['needs_attention'] == bool(result['findings'])
        for finding in result['findings']:
            assert all(isinstance(finding[key], str) and re.fullmatch(r'[a-z0-9_:-]{1,100}', finding[key])
                       for key in ('session', 'reason'))
        return result
    except subprocess.TimeoutExpired:
        return unknown('probe_timeout')
    except (OSError, ValueError, KeyError, TypeError, AssertionError):
        return unknown('probe_output_invalid')


def checkpoint(report, directory):
    directory = pathlib.Path(directory)
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    key_data = '\n'.join(sorted({f['session'] + ':' + f['reason'] for f in report['findings']}))
    key = hashlib.sha256(key_data.encode()).hexdigest()[:24] if key_data else ''
    state_path = directory / 'state.json'
    with (directory / 'state.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        previous = json.loads(state_path.read_text()) if state_path.exists() else {}
        notify = bool(key and previous.get('notification_key') != key)
        state = {'notification_key': key, 'status': report['status'],
                 'checked_at': datetime.datetime.now(datetime.timezone.utc).isoformat()}
        temporary = None
        try:
            with tempfile.NamedTemporaryFile(mode='w', dir=directory, delete=False) as file:
                temporary = file.name
                os.fchmod(file.fileno(), 0o600)
                json.dump(state, file)
                file.flush()
                os.fsync(file.fileno())
            os.replace(temporary, state_path)
            temporary = None
        finally:
            if temporary:
                os.unlink(temporary)
    return {**report, 'notification_key': key, 'notify': notify,
            'checked_at': state['checked_at']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host', required=True)
    parser.add_argument('--expected-sessions', type=int, default=2)
    parser.add_argument('--state-dir', type=pathlib.Path,
                        default=pathlib.Path.home() / '.local/state/discord-health')
    args = parser.parse_args()
    report = run_check(args.host, args.expected_sessions)
    try:
        result = checkpoint(report, args.state_dir)
    except Exception:
        result = {**unknown('monitor_state_unavailable'), 'notify': True}
    print(json.dumps(result, sort_keys=True))


if __name__ == '__main__':
    main()
