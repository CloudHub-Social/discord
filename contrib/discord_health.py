#!/usr/bin/env python3
"""Bounded, read-only Discord gateway probe. Never emits raw journal messages.

Exit 0: quiet (healthy, startup grace, or short recovery); 1: actionable condition;
2: unknown/collection failure. This is not an end-to-end message-delivery test.
"""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import re
import selectors
import subprocess
import sys
import time

SERVICE = 'matrix-mautrix-discord.service'
CONTAINER = 'matrix-mautrix-discord'
CONFIG = pathlib.Path('/matrix/mautrix-discord/config/config.yaml')
STALE_SECONDS = 300
STORM_SECONDS = 900
STORM_COUNT = 6
STABLE_SECONDS = 30
ANSI = re.compile(r'\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-_])')
LEVEL = re.compile(r'(?:^|\s)(DBG|DEBUG|INF|INFO|WRN|WARN|ERR|ERROR|FTL|FATAL|PNC|PANIC)(?:\s|\])')
IDENTITY = re.compile(r'(?:^|\s)user_id=(?:"([^"\r\n]+)"|([^\s]+))')
PREFIXES = {
    'Discord connection ready': 'ready',
    'Discord connection resumed': 'ready',
    'Disconnected from Discord': 'disconnect',
    'Connecting to discord': 'connecting',
    'Disconnecting session manually': 'manual',
    'Got logged out from Discord due to invalid token': 'invalid_auth',
    'Discord gateway reconnect attempt failed': 'connect_failed',
}
DIAGNOSTIC = re.compile(r'^gateway_diagnostic kind=(connect_error|read_error|heartbeat_write_error|heartbeat_ack_timeout|server_reconnect|invalid_session) close_code=([0-9]{1,5}) error_class=(none|other|websocket_close|transport|timeout|eof)(?=\s|$)')


class ProbeError(Exception):
    """Messages are fixed local error codes, never external exception text."""


def command(args, label, limit=4 * 1024 * 1024, timeout=12):
    deadline = time.monotonic() + timeout
    proc = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            env={**os.environ, 'LC_ALL': 'C', 'SYSTEMD_COLORS': '0'})
    out, size, stderr = [], 0, False
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(proc.stdout, selectors.EVENT_READ, 'out')
            selector.register(proc.stderr, selectors.EVENT_READ, 'err')
            while selector.get_map():
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise ProbeError(label + '_timeout')
                for key, _ in selector.select(min(remaining, 0.1)):
                    data = os.read(key.fileobj.fileno(), 65536)
                    if not data:
                        selector.unregister(key.fileobj)
                        continue
                    size += len(data)
                    if size > limit:
                        raise ProbeError(label + '_output_limit')
                    if key.data == 'out':
                        out.append(data)
                    else:
                        stderr = True
        proc.wait(timeout=max(0.01, deadline - time.monotonic()))
        if proc.returncode or stderr:
            raise ProbeError(label + '_failed')
        return b''.join(out).decode('utf-8', errors='strict')
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=2)
        proc.stdout.close()
        proc.stderr.close()


def parse_record(row, invocation):
    if row.get('_SYSTEMD_UNIT') != SERVICE or row.get('_SYSTEMD_INVOCATION_ID') != invocation:
        raise ProbeError('journal_scope_mismatch')
    stamp = int(row['__REALTIME_TIMESTAMP']) / 1e6
    if stamp <= 0:
        raise ProbeError('journal_timestamp_invalid')
    text = row['MESSAGE']
    if isinstance(text, list) and all(type(v) is int and 0 <= v <= 255 for v in text):
        text = bytes(text).decode('utf-8', errors='strict')
    if not isinstance(text, str):
        raise ProbeError('journal_message_invalid')
    clean = ANSI.sub('', text).strip()
    if clean.startswith('{'):
        obj = json.loads(clean)
        body, identity, component = obj.get('message'), obj.get('user_id'), obj.get('component')
        if not isinstance(body, str):
            raise ProbeError('journal_message_invalid')
    else:
        level = LEVEL.search(clean[:200])
        if not level:
            return None
        body = re.sub(r'^\S+\.go:\d+(?::[^\s]+)?\s*(?:>\s*)?', '', clean[level.end():].lstrip())
        if body.startswith('> '):
            body = body[2:]
        match = IDENTITY.search(clean)
        identity = (match.group(1) or match.group(2)) if match else None
        component = 'discordgo' if re.search(r'(?:^|\s)component=(?:"discordgo"|discordgo)(?:\s|$)', clean) else None
    event = None
    if component == 'discordgo':
        if re.match(r'^gateway_health heartbeat_ack=true(?:\s|$)', body):
            event = {'kind': 'ack'}
        else:
            match = DIAGNOSTIC.match(body)
            if match:
                event = {'kind': 'diagnostic', 'cause': match[1],
                         'close_code': int(match[2]), 'error_class': match[3]}
    for prefix, kind in PREFIXES.items():
        if body.startswith(prefix):
            event = {'kind': kind}
            if kind == 'disconnect' and ('manual disconnect' in body or 'just logged out' in body):
                event['kind'] = 'manual'
            break
    if event is None:
        return None
    if not isinstance(identity, str) or not identity or len(identity) > 512:
        raise ProbeError('journal_identity_missing')
    event.update(timestamp=stamp, session=hashlib.sha256(identity.encode()).hexdigest()[:12])
    return event


def assess(events, now, started, expected=2):
    if expected < 1 or started > now + 5:
        raise ProbeError('assessment_arguments_invalid')
    grouped = {}
    for item in sorted(events, key=lambda x: x['timestamp']):
        if item['timestamp'] > now + 5:
            raise ProbeError('journal_timestamp_future')
        if item['timestamp'] >= started:
            grouped.setdefault(item['session'], []).append(item)
    findings, sessions = [], {}
    startup = now - started < STALE_SECONDS

    def finding(session, reason, unknown=False):
        findings.append({'session': session, 'reason': reason, 'unknown': unknown})

    for session, items in grouped.items():
        last = {}
        failure_since = None
        ready_since = None
        auth_failed = heartbeat_failed = False
        heartbeat_recovery_since = None
        for item in items:
            kind, stamp = item['kind'], item['timestamp']
            if last.get('ack') is not None and stamp - last['ack'] >= STALE_SECONDS:
                heartbeat_failed = True
                heartbeat_recovery_since = None
            if kind == 'manual':
                failure_since = ready_since = None
                auth_failed = heartbeat_failed = False
                heartbeat_recovery_since = None
                last.pop('ack', None)
            elif kind in ('disconnect', 'connecting', 'connect_failed', 'invalid_auth'):
                if failure_since is None:
                    failure_since = stamp
                ready_since = None
            elif kind == 'ready':
                ready_since = stamp
            elif kind == 'ack' and ready_since is not None:
                if heartbeat_failed and heartbeat_recovery_since is None:
                    heartbeat_recovery_since = stamp
                stable_since = max(ready_since, heartbeat_recovery_since or ready_since)
                if stamp - stable_since >= STABLE_SECONDS:
                    # Require current-session ACK evidence after the stability
                    # window; a stale-heartbeat episode also needs ACKs spanning
                    # that window, not one late ACK on an old READY.
                    failure_since = None
                    auth_failed = heartbeat_failed = False
                    heartbeat_recovery_since = None
            if kind == 'invalid_auth' or item.get('close_code') == 4004:
                auth_failed = True
                ready_since = None
            last[item['kind']] = item['timestamp']
            if item.get('close_code') == 4004:
                last['invalid_auth'] = item['timestamp']
        ack = last.get('ack', 0)
        lifecycle = [v for v in items if v['kind'] in ('ready', 'manual', 'disconnect', 'connecting', 'connect_failed', 'invalid_auth')]
        current = lifecycle[-1]['kind'] if lifecycle else None
        storms = sum(v['kind'] == 'disconnect' and v['timestamp'] >= now - STORM_SECONDS for v in items)
        state = {'status': 'healthy', 'heartbeat_age_seconds': round(now - ack, 1) if ack else None,
                 'disconnects_last_15_minutes': storms}
        causes = [v for v in items if v['kind'] == 'diagnostic']
        if causes:
            state['last_cause'] = {k: causes[-1][k] for k in ('cause', 'close_code', 'error_class')}
        sessions[session] = state
        if current == 'manual':
            state['status'] = 'intentionally_disconnected'
            continue
        if auth_failed:
            finding(session, 'invalid_credentials')
            state['status'] = 'unhealthy'
        if storms >= STORM_COUNT:
            finding(session, 'reconnect_storm')
            state['status'] = 'unhealthy'
        if failure_since is not None:
            age = now - failure_since
            state['outage_age_seconds'] = round(age, 1)
            if age >= STALE_SECONDS:
                finding(session, 'recovery_stalled')
                state['status'] = 'unhealthy'
            elif state['status'] == 'healthy':
                state['status'] = 'recovering'
        if heartbeat_failed or (ack and now - ack >= STALE_SECONDS):
            finding(session, 'heartbeat_stale')
            state['status'] = 'unhealthy'
        elif not ack:
            if startup:
                if state['status'] == 'healthy':
                    state['status'] = 'starting'
            else:
                finding(session, 'heartbeat_missing', unknown=True)
                if state['status'] != 'unhealthy':
                    state['status'] = 'unknown'
    if len(sessions) < expected and not startup:
        finding('bridge', 'session_coverage_missing', unknown=True)
    if findings:
        status = 'alert' if any(not f['unknown'] for f in findings) else 'unknown'
    elif startup and len(sessions) < expected:
        status = 'starting'
    elif any(v['status'] == 'recovering' for v in sessions.values()):
        status = 'recovering'
    elif any(v['status'] == 'starting' for v in sessions.values()):
        status = 'starting'
    else:
        status = 'healthy'
    return {'status': status, 'needs_attention': bool(findings), 'findings': findings,
            'sessions': sessions, 'expected_sessions': expected}


def notification_key(report):
    keys = sorted({f[ 'session'] + ':' + f['reason'] for f in report.get('findings', [])})
    return hashlib.sha256('\n'.join(keys).encode()).hexdigest()[:24] if keys else ''


def unit_snapshot():
    names = ('ActiveState', 'SubState', 'InvocationID', 'NRestarts')
    raw = command(['systemctl', 'show', SERVICE, '--no-pager', '--property=' + ','.join(names)], 'systemd')
    result = dict(line.split('=', 1) for line in raw.splitlines() if '=' in line)
    if set(result) != set(names):
        raise ProbeError('systemd_properties_missing')
    return result


def container_snapshot():
    template = '{"id":{{json .Id}},"running":{{json .State.Running}},"started":{{json .State.StartedAt}},"restarts":{{json .RestartCount}},"revision":{{json (index .Config.Labels "org.opencontainers.image.revision")}}}'
    result = json.loads(command(['docker', 'inspect', '--format', template, CONTAINER], 'container', limit=8192))
    if not re.fullmatch(r'[0-9a-f]{64}', result.get('id', '')) or not re.fullmatch(r'[0-9a-f]{40}', result.get('revision', '')):
        raise ProbeError('container_identity_invalid')
    return result


def presence_snapshot():
    with CONFIG.open('rb') as file:
        data = file.read(2 * 1024 * 1024 + 1)
    if len(data) > 2 * 1024 * 1024:
        raise ProbeError('configuration_output_limit')
    flags = {}
    for name in ('sync_matrix_presence_to_discord', 'sync_matrix_status_to_discord'):
        lines = [line for line in data.decode().splitlines() if re.match(r'^\s*' + name + r':', line)]
        if len(lines) != 1:
            raise ProbeError('presence_configuration_ambiguous')
        match = re.fullmatch(r'\s*' + name + r':\s*(true|false)\s*(?:#.*)?', lines[0])
        if not match:
            raise ProbeError('presence_configuration_ambiguous')
        flags[name] = match[1] == 'true'
    return {'flags': flags, 'sha256': hashlib.sha256(data).hexdigest()}


def journal_records(invocation, recent):
    names = list(PREFIXES)
    if recent:
        names.extend(['gateway_health', 'gateway_diagnostic'])
    args = ['journalctl', '--no-pager', '--quiet', '--all', '--output=json',
            '--output-fields=__REALTIME_TIMESTAMP,__CURSOR,_SYSTEMD_INVOCATION_ID,_SYSTEMD_UNIT,MESSAGE',
            '--grep=' + '|'.join(names), '--lines=' + ('2001' if recent else '256'),
            '_SYSTEMD_UNIT=' + SERVICE, '_SYSTEMD_INVOCATION_ID=' + invocation]
    if recent:
        args.append('--since=-20min')
    lines = command(args, 'journal', limit=4 * 1024 * 1024).splitlines()
    if recent and len(lines) > 2000:
        raise ProbeError('journal_window_truncated')
    return [json.loads(line) for line in lines]


def collect(expected=2):
    unit = unit_snapshot()
    if unit['ActiveState'] != 'active' or unit['SubState'] != 'running':
        return {'status': 'alert', 'needs_attention': True, 'findings': [{'session': 'bridge', 'reason': 'service_down', 'unknown': False}], 'unit': unit}
    invocation = unit['InvocationID']
    if not re.fullmatch(r'[0-9a-f]{32}', invocation):
        raise ProbeError('service_invocation_missing')
    container = container_snapshot()
    if not container['running']:
        raise ProbeError('container_not_running')
    presence = presence_snapshot()
    records = journal_records(invocation, False) + journal_records(invocation, True)
    events, seen = [], set()
    for row in records:
        key = row.get('__CURSOR') or json.dumps(row, sort_keys=True)
        if key in seen:
            continue
        seen.add(key)
        event = parse_record(row, invocation)
        if event:
            events.append(event)
    if unit_snapshot() != unit or container_snapshot() != container or presence_snapshot() != presence:
        raise ProbeError('deployment_changed_during_snapshot')
    now = time.time()
    started = datetime.datetime.fromisoformat(container['started'].replace('Z', '+00:00')).timestamp()
    result = assess(events, now, started, expected)
    if any(presence['flags'].values()):
        result.update(status='alert', needs_attention=True)
        result['findings'].append({'session': 'bridge', 'reason': 'outgoing_presence_enabled', 'unknown': False})
    result.update(captured_at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                  running_revision=container['revision'], container=container, unit=unit,
                  configuration=presence, parsed_records=len(events),
                  note='Gateway ACK and lifecycle evidence; not an end-to-end message-delivery test.')
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--expected-sessions', type=int, default=2)
    args = parser.parse_args()
    try:
        if not 1 <= args.expected_sessions <= 100:
            raise ProbeError('expected_sessions_invalid')
        result = collect(args.expected_sessions)
    except Exception as exc:
        reason = str(exc) if isinstance(exc, ProbeError) else 'collection_failed'
        result = {'status': 'unknown', 'needs_attention': True,
                  'findings': [{'session': 'bridge', 'reason': reason, 'unknown': True}],
                  'captured_at': datetime.datetime.now(datetime.timezone.utc).isoformat()}
    result['notification_key'] = notification_key(result)
    print(json.dumps(result, sort_keys=True))
    return 2 if result['status'] == 'unknown' else int(result['needs_attention'])


if __name__ == '__main__':
    sys.exit(main())
