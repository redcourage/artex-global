"""Run an actual ARTEX task against disposable local fixtures using Codex.

Requires Docker, an ARTEX binary, and an authenticated Codex CLI. Uses subscription
quota. All owned processes and the disposable database are stopped on exit.
"""
import argparse
import http.server
import json
import math
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', required=True)
    parser.add_argument('--planner-model', help='Available Codex model ID; defaults to account default or first available')
    parser.add_argument('--worker-model', help='Available Codex model ID; defaults to account default or first available')
    parser.add_argument('--timeout', type=int, default=360)
    return parser.parse_args(argv)


def discover_models(port, token):
    request = urllib.request.Request(f'http://127.0.0.1:{port}/v1/models',
                                     headers={'Authorization': 'Bearer ' + token})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.loads(response.read())
    except (OSError, ValueError) as exc:
        raise RuntimeError('Unable to list available Codex subscription models: ' + str(exc)) from exc


def select_models(response, planner=None, worker=None):
    models = response.get('data') if isinstance(response, dict) else None
    if not isinstance(models, list) or not models or any(
            not isinstance(m, dict) or not isinstance(m.get('id'), str) or not m['id'].strip()
            for m in models):
        raise ValueError('Codex returned no usable available models')
    available = [m['id'] for m in models]
    default = next((m['id'] for m in models if m.get('default') is True), available[0])
    selected = {}
    for role, requested in [('planner', planner), ('worker', worker)]:
        if requested is not None and requested not in available:
            raise ValueError(f'{role} model unavailable: {requested!r}; available models: ' + ', '.join(available))
        selected[role] = default if requested is None else requested
    return selected


def model_costs_consistent(models, rows):
    """Reconcile priced snapshots and explicitly unpriced usage without guessing rates."""
    if not models or not rows or any(type(r.get('usage_known')) is not bool for r in rows):
        return False
    if any(not r['usage_known'] and r.get('cost_snapshot') is not None for r in rows):
        return False
    if len({m['model'] for m in models}) != len(models) or {m['model'] for m in models} != {r['model'] for r in rows}:
        return False
    amounts = ('input_usd', 'cache_read_usd', 'cache_write_usd', 'output_usd', 'total_usd',
               'without_cache_usd', 'cache_read_savings_usd', 'cache_write_premium_usd', 'net_savings_usd')
    for model in models:
        calls = [r for r in rows if r['model'] == model['model']]
        priced = [r['cost_snapshot'] for r in calls if r.get('cost_snapshot') is not None]
        if (model.get('calls') != len(calls) or model.get('priced_calls') != len(priced) or
                model.get('unpriced_calls') != len(calls) - len(priced) or
                model.get('usage_missing_calls') != sum(not r['usage_known'] for r in calls)):
            return False
        cost = model.get('cost')
        if not priced:
            if cost is not None:
                return False
            continue
        if not isinstance(cost, dict):
            return False
        for item in [cost, *priced]:
            if (item.get('currency') != 'USD' or item.get('basis') != 'standard_api_equivalent' or
                    not item.get('price_version') or not item.get('source_url')):
                return False
            if any(not isinstance(item.get(key), (int, float)) or not math.isfinite(item[key]) for key in amounts):
                return False
        if any(not math.isclose(cost[key], sum(p[key] for p in priced), rel_tol=1e-9, abs_tol=1e-12) for key in amounts):
            return False
    return True


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def install_shutdown_handlers():
    def shutdown(signum, frame):
        raise SystemExit(128 + signum)
    signal.signal(signal.SIGTERM, shutdown)


def stop_process_group(process):
    # Children may still exist even if the group leader has already exited.
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        process.wait()
        return
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        pass
    finally:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()


def main(args):
    root = Path(tempfile.mkdtemp(prefix='artex-codex-e2e-'))
    print('ARTIFACT_DIR=' + str(root), flush=True)
    started = time.monotonic()
    owned = []
    handles = []
    name = 'artex-codex-probe-' + secrets.token_hex(4)
    db_started = False
    fixture = None
    auth = ''
    server_port, bridge_port = free_port(), free_port()
    base = f'http://127.0.0.1:{server_port}'
    password = secrets.token_hex(16)
    token = secrets.token_hex(24)
    samples = {'alpha': {'value': 17, 'marker': 'alpha-' + secrets.token_hex(8)},
               'beta': {'value': 25, 'marker': 'beta-' + secrets.token_hex(8)}}
    requests = []

    def api(method, path, body=None):
        headers = {'Content-Type': 'application/json'}
        if auth:
            headers['Authorization'] = 'Bearer ' + auth
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(base + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=20) as response:
                raw = response.read()
                if 'application/json' in response.headers.get('Content-Type', ''):
                    return json.loads(raw)
                return raw.decode()
        except urllib.error.HTTPError as exc:
            raise RuntimeError(f'{method} {path}: {exc.code} {exc.read().decode()}') from exc

    def launch(command, env, log):
        handle = (root / log).open('wb')
        handles.append(handle)
        process = subprocess.Popen(command, env=env, stdout=handle, stderr=subprocess.STDOUT,
                                   start_new_session=True)
        owned.append(process)
        return process

    def wait_ready(url, process):
        for _ in range(100):
            if process.poll() is not None:
                raise RuntimeError(f'Process exited before ready: {process.args[0]}')
            try:
                with urllib.request.urlopen(url, timeout=1):
                    return
            except (OSError, urllib.error.URLError):
                time.sleep(.2)
        raise RuntimeError('Startup timeout: ' + url)

    def sql(query):
        out = subprocess.check_output(['docker', 'exec', '-i', name, 'psql', '-U', 'artex',
                                       '-d', 'artex', '-At', '-v', 'ON_ERROR_STOP=1'], input=query.encode())
        return out.decode().strip()

    class Fixture(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            key = self.path.strip('/').split('?')[0]
            requests.append({'path': self.path, 'time': time.time()})
            body = samples.get(key, {'paths': ['/alpha', '/beta'], 'purpose': 'local integration fixture'})
            raw = json.dumps(body).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *unused):
            pass

    try:
        env = os.environ.copy()
        env['ARTEX_BRIDGE_TOKEN'] = token
        bridge = launch(['python3', str(Path(__file__).with_name('bridge.py')), '--port', str(bridge_port),
                         '--output', str(root / 'bridge')], env, 'bridge.log')
        wait_ready(f'http://127.0.0.1:{bridge_port}/health', bridge)
        available_models = discover_models(bridge_port, token)
        selected = select_models(available_models, args.planner_model, args.worker_model)
        args.planner_model, args.worker_model = selected['planner'], selected['worker']
        (root / 'models.json').write_text(json.dumps({'available': available_models['data'],
                                                    'selected': selected}, indent=2))
        print(json.dumps({'available_models': available_models['data'], 'models': selected}), flush=True)

        result = subprocess.run(['docker', 'run', '-d', '--rm', '--name', name,
            '-e', 'POSTGRES_USER=artex', '-e', 'POSTGRES_DB=artex', '-e', 'POSTGRES_PASSWORD=' + password,
            '-p', '127.0.0.1::5432', 'postgres:16-alpine'], check=True, capture_output=True, text=True)
        db_started = True
        port = subprocess.check_output(['docker', 'port', name, '5432'], text=True).strip().rsplit(':', 1)[1]
        for _ in range(60):
            ready = subprocess.run(['docker', 'exec', name, 'pg_isready', '-U', 'artex'], capture_output=True)
            if ready.returncode == 0:
                break
            time.sleep(.3)
        else:
            raise RuntimeError('PostgreSQL did not become ready')

        fixture = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Fixture)
        threading.Thread(target=fixture.serve_forever, daemon=True).start()
        target = f'http://127.0.0.1:{fixture.server_address[1]}'
        for key in ('ANTHROPIC_API_KEY', 'OPENAI_API_KEY', 'ARTEX_LLM_PROVIDER', 'ARTEX_LLM_MODEL',
                    'ARTEX_LLM_BASE_URL', 'ARTEX_LLM_PROXY', 'ARTEX_BRIDGE_TOKEN'):
            env.pop(key, None)
        env.update({'ARTEX_PG_DSN': f'postgres://artex:{password}@127.0.0.1:{port}/artex?sslmode=disable',
                    'ARTEX_CONFIG': str(root / 'unused-config.json'), 'ARTEX_ADMIN_PASSWORD': password,
                    'ARTEX_SKILL_DIR': str(root / 'empty-skills'), 'ARTEX_LANG': 'en'})
        backend = launch([str(Path(args.binary).resolve()), '-addr', f'127.0.0.1:{server_port}',
                          '-proxy', '', '-data', str(root / 'data')], env, 'artex.log')
        wait_ready(base + '/api/auth/status', backend)
        auth = api('POST', '/api/auth/login', {'username': 'ARTEX', 'password': password})['token']
        profiles = {}
        for role, model in [('planner', args.planner_model), ('worker', args.worker_model)]:
            profiles[role] = api('POST', '/api/llm/profiles', {
                'name': 'Codex subscription probe ' + role, 'format': 'openai', 'model': model,
                'base_url': f'http://127.0.0.1:{bridge_port}/v1', 'api_key': token, 'streaming': False,
                'session_header_key': 'X-Artex-Codex-Session', 'reasoning_effort': 'low',
                'context_window_k': 128, 'pool_exclude': True})['id']
        api('POST', '/api/llm/profiles/active', {'id': profiles['planner']})
        for role in ['planner', 'goals', 'worker', 'mainagent']:
            api('PUT', f'/api/agents/{role}/config', {
                'llm_profile_id': profiles['worker' if role == 'worker' else 'planner'],
                'max_turns': 30, 'run_seconds': 180, 'web_search': False, 'interactive_shell': False})
        api('PUT', '/api/settings', {'workers': 2, 'web_search_enabled': False, 'llm_pool_enabled': False})
        goal = (
            f'Local integration validation only. The sole authorized target is {target}. '
            'Do not scan ports, enumerate other hosts, exploit anything, or access other systems. '
            f'Fetch {target}/alpha and {target}/beta and record each returned value and unpredictable marker '
            'as separate facts with evidence. The Planner must delegate the two fetches as separate worker '
            'intents. After those facts exist, delegate a follow-up worker intent to read the previous '
            'worker facts/traces and record an aggregate fact containing the sum of both values and both '
            'exact markers. No further HTTP fetch is needed for the aggregate. Prove the goal only after '
            'the aggregate fact exists. This is a finite functionality check, not a vulnerability hunt. '
            'The expected marker strings are intentionally not supplied; obtain them from actual tool output.')
        task = api('POST', '/api/tasks', {'name': 'Codex subscription actual engine probe',
            'description': 'Validate local fixture observations and cross-worker handoff',
            'goal': goal, 'llm_profile_ids': [profiles['planner']],
            'timeout_seconds': args.timeout, 'seed_first_intent': False, 'coverage_enabled': False})
        task_id = str(task['id'])
        print(json.dumps({'task_id': task_id, 'target': target, 'models': {
            'planner': args.planner_model, 'worker': args.worker_model}}), flush=True)
        last = None
        deadline = time.monotonic() + args.timeout + 45
        while time.monotonic() < deadline:
            task = api('GET', '/api/tasks/' + task_id)
            status = task.get('status')
            counts = sql("SELECT kind || ':' || count(*) FROM exploration_nodes GROUP BY kind ORDER BY kind;")
            current = (status, counts)
            if current != last:
                print(json.dumps({'status': status, 'nodes': counts}), flush=True)
                last = current
            if status in ('done', 'failed', 'timeout'):
                break
            if bridge.poll() is not None or backend.poll() is not None:
                raise RuntimeError('Probe service exited')
            time.sleep(2)
        else:
            raise RuntimeError('Probe deadline exceeded')

        nodes = json.loads(sql("SELECT coalesce(json_agg(n),'[]') FROM (SELECT id,kind,state,payload,owner FROM exploration_nodes ORDER BY id) n;"))
        activity = json.loads(sql("SELECT coalesce(json_agg(a),'[]') FROM (SELECT id,worker,kind,tool,is_error,summary,detail FROM activity ORDER BY id) a;"))
        report = api('GET', '/api/report?task=' + task_id)
        facts = [n for n in nodes if n['kind'] == 'fact']
        facts_text = json.dumps(facts)
        worker_tools = [a for a in activity if (a['worker'] or '').startswith('work') and a['kind']=='tool_use']
        intent_calls = [a for a in activity if a['worker']=='planner' and a['kind']=='tool_use' and a['tool']=='add_intent']
        rounds = [a for a in activity if a['worker']=='planner' and a['kind']=='round']
        evidence = {'task': task, 'nodes': nodes, 'activity': activity, 'fixture_requests': requests,
                    'fixtures': samples, 'duration_seconds': round(time.monotonic()-started, 2),
                    'models': {'planner':args.planner_model,'worker':args.worker_model}}
        model_usage = api('GET', '/api/llm/records/by-model?task=' + task_id)['models']
        usage_rows = json.loads(sql("SELECT coalesce(json_agg(u),'[]') FROM (SELECT model,worker,session_id,usage_source,metering_id,usage_known,input_tokens,output_tokens,cache_read,cache_write,reasoning_tokens,cost_snapshot FROM llm_usage ORDER BY id) u;"))
        evidence['model_usage'] = model_usage
        evidence['usage_rows'] = usage_rows
        usage_totals = {}
        for line in (root / 'bridge' / 'bridge.jsonl').read_text().splitlines():
            row = json.loads(line)
            if row['event'] == 'usage':
                usage_totals[row['thread']] = row['total']
        evidence['codex_usage_totals'] = usage_totals
        (root / 'evidence.json').write_text(json.dumps(evidence, ensure_ascii=False, indent=2))
        (root / 'report.md').write_text(report)
        checks = {'task_done': task.get('status')=='done', 'planner_created_intents': bool(intent_calls),
            'multiple_planner_rounds': len(rounds)>=2, 'workers_executed_tools': bool(worker_tools),
            'real_fixture_values_recorded': all(v['marker'] in facts_text for v in samples.values()),
            'three_or_more_intents': len([n for n in nodes if n['kind']=='intent'])>=3,
            'aggregate_fact': any('42' in json.dumps(n['payload']) and
                                  all(v['marker'] in json.dumps(n['payload']) for v in samples.values()) for n in facts),
            'all_goals_met': bool([n for n in nodes if n['kind']=='goal']) and
                             all(n['state']=='met' for n in nodes if n['kind']=='goal'),
            'codex_usage_recorded': bool(usage_rows) and all(u['usage_source']=='codex_subscription' for u in usage_rows),
            'usage_matches_codex': bool(usage_totals) and all(
                sum(u[column] for u in usage_rows) == sum(t[field] for t in usage_totals.values())
                for column,field in [('input_tokens','inputTokens'),('output_tokens','outputTokens'),
                                     ('cache_read','cachedInputTokens'),('cache_write','cacheWriteInputTokens')]),
            'model_costs_consistent': model_costs_consistent(model_usage, usage_rows),
            'cached_tokens_recorded': sum(u['cache_read'] for u in usage_rows) > 0}
        (root / 'checks.json').write_text(json.dumps(checks, indent=2))
        print(json.dumps({'checks': checks, 'duration_seconds': evidence['duration_seconds'],
                          'artifacts': str(root)}), flush=True)
        if not all(checks.values()):
            raise RuntimeError('Actual ARTEX probe did not satisfy every check')
    finally:
        # Preserve diagnostics even when a setup request or inference fails.
        (root / 'fixture-requests.json').write_text(json.dumps(requests, indent=2))
        for process in reversed(owned):
            stop_process_group(process)
        if fixture is not None:
            fixture.shutdown()
            fixture.server_close()
        if db_started:
            subprocess.run(['docker', 'stop', '--time', '3', name], capture_output=True)
        for handle in handles:
            handle.close()
        print('Probe services stopped; artifacts retained at ' + str(root), flush=True)


if __name__ == '__main__':
    install_shutdown_handlers()
    main(parse_args())
