import asyncio
import tempfile
import unittest
from pathlib import Path

from app_server import AppServer
from protocol import SubscriptionLimit, ToolReviewRequired
from quota import QuotaState
from test_app_server import State
from sessions import replay_start
from sessions import Bridge
from test_recovery import Peer, body


class QuotaPeer(Peer):
    def __init__(self, output, recover=False):
        super().__init__()
        self.quota = QuotaState(output)
        self.recover = recover

    async def rpc(self, method, params):
        if method == 'turn/start':
            state = self.sessions[params['threadId']]
            state.pending['write-1'] = 1
            await state.events.put({'kind': 'tool', 'id': 'write-1', 'name': 'Write', 'arguments': {'value': 42}})
            return {'turn': {'id': 't'}}
        return await super().rpc(method, params)

    async def send(self, message):
        self.sent.append(message)
        state = next(iter(self.sessions.values()))
        if self.recover:
            await state.events.put({'kind': 'text', 'text': 'done from recorded result'})
        else:
            self.quota.block()
            await state.events.put({'kind': 'error', 'subscription_limit': True})


class QuotaTests(unittest.IsolatedAsyncioTestCase):
    async def test_new_read_cannot_expose_a_completed_write_during_recovery(self):
        class ReadThenWrite(QuotaPeer):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    state = self.sessions[params['threadId']]
                    # Both arrive before the first response: batch gathering
                    # must not bypass the completed-action guard either.
                    state.pending.update({'fresh-read': 2, 'repeat-write': 3})
                    await state.events.put({'kind': 'tool', 'id': 'fresh-read', 'name': 'Read', 'arguments': {'path': 'new'}})
                    await state.events.put({'kind': 'tool', 'id': 'repeat-write', 'name': 'Write', 'arguments': {'value': 42}})
                    return {'turn': {'id': 't'}}
                return await super().rpc(method, params)
            async def send(self, message):
                self.sent.append(message)
        with tempfile.TemporaryDirectory() as tmp:
            app = ReadThenWrite(Path(tmp))
            bridge = Bridge(app)
            self.addAsyncCleanup(bridge.close)
            request = body([{'role': 'user', 'content': 'do work'},
                {'role': 'assistant', 'tool_calls': [{'id': 'old-write', 'type': 'function',
                    'function': {'name': 'Write', 'arguments': '{"value":42}'}}]},
                {'role': 'tool', 'tool_call_id': 'old-write', 'content': 'saved once'},
                {'role': 'user', 'content': '[ARTEX_AUTOMATIC_CONTINUATION] Continue.'}])
            request['tools'].append({'type': 'function', 'function': {'name': 'Write', 'parameters': {'type': 'object'}}})
            response = await bridge.complete('worker', request)
            calls = response['choices'][0]['message']['tool_calls']
            self.assertEqual([c['id'] for c in calls], ['fresh-read'])
            request['messages'] += [response['choices'][0]['message'],
                {'role': 'tool', 'tool_call_id': 'fresh-read', 'content': 'new observation'}]
            with self.assertRaises(ToolReviewRequired):
                await bridge.complete('worker', request)

    async def test_restart_replays_completed_write_internally_after_quota(self):
        with tempfile.TemporaryDirectory() as tmp:
            app = QuotaPeer(Path(tmp))
            bridge = Bridge(app)
            request = body()
            request['tools'][0]['function']['name'] = 'Write'
            first = await bridge.complete('worker', request)
            request['messages'] += [first['choices'][0]['message'],
                {'role': 'tool', 'tool_call_id': 'write-1', 'content': 'saved once'}]
            with self.assertRaises(SubscriptionLimit):
                await bridge.complete('worker', request)
            await bridge.close()
            app = QuotaPeer(Path(tmp), recover=True)
            bridge = Bridge(app)
            self.addAsyncCleanup(bridge.close)
            request['messages'].append({'role': 'user', 'content': '[ARTEX_AUTOMATIC_CONTINUATION] Continue unfinished work.'})
            with self.assertRaises(SubscriptionLimit):
                await bridge.complete('worker', request)
            self.assertEqual(app.calls, [])
            app.quota.update({'ordinaryUsageAllowed': True})
            response = await bridge.complete('worker', request)
            self.assertEqual(response['choices'][0]['message']['content'], 'done from recorded result')
            self.assertEqual(len(app.sent), 1)
            self.assertIn('saved once', app.sent[0]['result']['contentItems'][0]['text'])

    async def test_automatic_continuation_keeps_completed_tool_replay_scope(self):
        messages = [{'role': 'user', 'content': 'do work'},
                    {'role': 'assistant', 'content': 'working'},
                    {'role': 'user', 'content': '[ARTEX_AUTOMATIC_CONTINUATION] Continue unfinished work.'}]
        self.assertEqual(replay_start(messages), 0)
        messages.append({'role': 'user', 'content': 'new task'})
        self.assertEqual(replay_start(messages), 3)

    async def test_only_explicit_permission_recovers_and_latch_survives_restart(self):
        with tempfile.TemporaryDirectory() as tmp:
            q = QuotaState(Path(tmp))
            q.block()
            q = QuotaState(Path(tmp))
            self.assertTrue(q.blocked)
            for snapshot in ({}, {'rateLimits': {'primary': {'usedPercent': 0, 'resetsAt': 1}}},
                             {'ordinaryUsageAllowed': None}, {'ordinaryUsageAllowed': False}):
                q.update(snapshot)
                self.assertTrue(q.blocked)
            q.update({'ordinaryUsageAllowed': True})
            self.assertFalse(QuotaState(Path(tmp)).blocked)

    async def test_reset_hint_uses_latest_exhausted_window(self):
        with tempfile.TemporaryDirectory() as tmp:
            q = QuotaState(Path(tmp))
            q.block()
            q.update({'ordinaryUsageAllowed': False, 'rateLimits': {
                'primary': {'usedPercent': 100, 'resetsAt': 100},
                'secondary': {'usedPercent': 100, 'resetsAt': 200}}})
            self.assertEqual(q.status()['retry_at'], 200)
            self.assertFalse(q.status()['available'])

    async def test_quota_error_identity_survives_rpc_and_turn_notifications(self):
        with tempfile.TemporaryDirectory() as tmp:
            app = AppServer(Path(tmp))
            app.sessions['t'] = state = State()
            await app._dispatch({'method': 'turn/completed', 'params': {'threadId': 't',
                'turn': {'status': 'failed', 'error': {'codexErrorInfo': 'usageLimitExceeded'}}}})
            self.assertTrue((await state.events.get())['subscription_limit'])
            app.requests[1] = fut = asyncio.get_running_loop().create_future()
            await app._dispatch({'id': 1, 'error': {'code': -32000,
                'data': {'codexErrorInfo': 'usageLimitExceeded'}}})
            with self.assertRaises(SubscriptionLimit):
                await fut

    async def test_late_healthy_snapshot_cannot_clear_a_newer_limit(self):
        with tempfile.TemporaryDirectory() as tmp:
            app = AppServer(Path(tmp))
            async def ready():
                pass
            async def rpc(method, params):
                app.quota.block()  # quota error arrives while the read is in flight
                return {'ordinaryUsageAllowed': True}
            app.ensure_ready, app.rpc = ready, rpc
            status = await app.quota_status()
            self.assertFalse(status['available'])
            self.assertTrue(status['blocked'])
