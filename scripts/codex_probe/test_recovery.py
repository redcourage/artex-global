import asyncio
import copy
import json
import unittest

from bridge import Bridge, ProtocolError
from protocol import ServiceUnavailable, SessionConflict
from test_bridge import FakeApp


TOOLS = [{'type': 'function', 'function': {'name': 'Read', 'parameters': {'type': 'object'}}}]


def body(messages=None):
    return {'model': 'test-model', 'tools': copy.deepcopy(TOOLS),
            'messages': messages or [{'role': 'user', 'content': 'run'}]}


class Peer(FakeApp):
    def __init__(self):
        super().__init__()
        self.sent = []

    async def rpc(self, method, params):
        if method == 'thread/unsubscribe':
            self.calls.append((method, params))
            return {'status': 'unsubscribed'}
        return await super().rpc(method, params)

    async def send(self, message):
        self.sent.append(message)


class RecoveryTests(unittest.IsolatedAsyncioTestCase):
    async def test_chat_resume_after_tool_roundtrip_keeps_same_thread(self):
        class ChatPeer(Peer):
            async def rpc(self, method, params):
                if method == 'turn/start' and not self.sent:
                    self.calls.append((method, params))
                    state = self.sessions[params['threadId']]
                    state.pending['read-1'] = 1
                    await state.events.put({'kind': 'tool', 'id': 'read-1', 'name': 'Read',
                                            'arguments': {'file_path': '/tmp/fixture'}})
                    return {'turn': {'id': 'first'}}
                return await super().rpc(method, params)

            async def send(self, message):
                await super().send(message)
                state = next(iter(self.sessions.values()))
                state.active_turn = None
                await state.events.put({'kind': 'text', 'text': 'remembered 42'})

        app = ChatPeer()
        bridge = Bridge(app)
        self.addAsyncCleanup(bridge.close)
        request = body([{'role': 'user', 'content': 'read and remember 42'}])
        call = await bridge.complete('conv-1', request)
        request['messages'] += [call['choices'][0]['message'],
                                {'role': 'tool', 'tool_call_id': 'read-1', 'content': '42'}]
        answer = await bridge.complete('conv-1', request)
        self.assertEqual(answer['choices'][0]['message']['content'], 'remembered 42')
        request['messages'].append(answer['choices'][0]['message'])
        thread = bridge.states['conv-1'].thread
        # Norma persists tool inputs as json.RawMessage. Go's transcript writer
        # compacts it, so Resume sends compact JSON instead of the original spaces.
        request['messages'][1]['tool_calls'][0]['function']['arguments'] = '{"file_path":"/tmp/fixture"}'
        for text in ('what number?', 'continue again'):
            request['messages'].append({'role': 'user', 'content': text})
            result = await bridge.complete('conv-1', request)
            self.assertEqual(result['choices'][0]['message']['content'], 'answer')
            request['messages'].append(result['choices'][0]['message'])
            self.assertEqual(bridge.states['conv-1'].thread, thread)
        self.assertEqual(len(app.sent), 1)  # The historical Read was not re-executed.
        self.assertEqual(sum(method == 'thread/start' for method, _ in app.calls), 1)

    async def test_recovery_replay_ends_when_new_work_is_exposed(self):
        class WriteReadPeer(Peer):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    state = self.sessions[params['threadId']]
                    state.pending['write'] = 10
                    await state.events.put({'kind': 'tool', 'id': 'write', 'name': 'Write', 'arguments': {'text': 'NEW'}})
                    return {'turn': {'id': 't'}}
                return await super().rpc(method, params)

            async def send(self, message):
                await super().send(message)
                state = next(iter(self.sessions.values()))
                if message['id'] == 10:
                    state.pending['verify'] = 11
                    await state.events.put({'kind': 'tool', 'id': 'verify', 'name': 'Read', 'arguments': {'path': 'fixture'}})

        old_call = {'role': 'assistant', 'tool_calls': [{'id': 'old', 'type': 'function',
                    'function': {'name': 'Read', 'arguments': '{"path":"fixture"}'}}]}
        messages = [{'role': 'user', 'content': 'read, write NEW, then read to verify'}, old_call,
                    {'role': 'tool', 'tool_call_id': 'old', 'content': 'OLD'}]
        app = WriteReadPeer()
        bridge = Bridge(app, response_timeout=.01)
        request = body(messages)
        request['tools'].append({'type': 'function', 'function': {'name': 'Write', 'parameters': {'type': 'object'}}})
        write = await bridge.complete('w', request)
        request['messages'] += [write['choices'][0]['message'], {'role': 'tool', 'tool_call_id': 'write', 'content': 'Wrote NEW'}]
        verify = await bridge.complete('w', request)
        self.assertEqual(verify['choices'][0]['message']['tool_calls'][0]['id'], 'verify')
        self.assertEqual(len(app.sent), 1)
        await bridge.close()

    async def test_stale_reaper_cannot_remove_replacement_session(self):
        class SlowFirstRelease(Peer):
            def __init__(self):
                super().__init__()
                self.wait_thread = None
                self.releasing, self.proceed = asyncio.Event(), asyncio.Event()

            async def rpc(self, method, params):
                if method == 'thread/unsubscribe' and params['threadId'] == self.wait_thread:
                    self.releasing.set()
                    await self.proceed.wait()
                return await super().rpc(method, params)

        app = SlowFirstRelease()
        bridge = Bridge(app, idle_ttl=1)
        for key in ('a', 'b'):
            await bridge.complete(key, body())
        for state in bridge.states.values():
            state.last_touch -= 2
        first_key = next(iter(bridge.states))
        second_key = next(k for k in bridge.states if k != first_key)
        app.wait_thread = bridge.states[first_key].thread
        cleanup = asyncio.create_task(bridge.reap())
        await app.releasing.wait()
        await bridge.complete('c', body())
        await bridge.complete(second_key, body())
        replacement = bridge.states[second_key]
        app.proceed.set()
        await cleanup
        self.assertIs(bridge.states.get(second_key), replacement)
        self.assertEqual({s.thread for s in bridge.states.values()}, set(app.sessions))
        await bridge.close()

    async def test_timeout_then_late_failure_carries_usage_into_recovered_response(self):
        class MeteredPeer(Peer):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    state = self.sessions[params['threadId']]
                    state.usage.update({'inputTokens': 100, 'cachedInputTokens': 40,
                                        'outputTokens': 20, 'reasoningOutputTokens': 0})
                return await super().rpc(method, params)
        app = MeteredPeer()
        app.finish = False
        bridge = Bridge(app, response_timeout=.01)
        with self.assertRaises(ServiceUnavailable):
            await bridge.complete('w', body())
        state = bridge.states['w']
        await state.events.put({'kind': 'error', 'error': 'late failure', 'retryable': True})
        await asyncio.gather(state.request_task, return_exceptions=True)
        app.finish = True
        response = await bridge.complete('w', body())
        self.assertEqual(response['usage']['prompt_tokens'], 200)
        self.assertEqual(response['usage']['completion_tokens'], 40)
        self.assertEqual(len({s['id'] for s in response['artex_usage']['samples']}), 2)
        self.assertEqual(response, await bridge.complete('w', body()))

    async def test_unreported_failed_attempt_stays_missing_after_recovery(self):
        app = Peer()
        app.finish = False
        bridge = Bridge(app, response_timeout=.01)
        with self.assertRaises(ServiceUnavailable):
            await bridge.complete('w', body())
        state = bridge.states['w']
        await state.events.put({'kind': 'error', 'error': 'late failure', 'retryable': True})
        await asyncio.gather(state.request_task, return_exceptions=True)
        app.finish = True
        response = await bridge.complete('w', body())
        self.assertEqual(len(response['artex_usage']['missing_ids']), 2)
        self.assertNotIn('usage', response)

    async def test_new_user_turn_after_eviction_can_repeat_an_old_tool(self):
        class ReadPeer(Peer):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    state = self.sessions[params['threadId']]
                    state.pending['new-call'] = 99
                    await state.events.put({'kind': 'tool', 'id': 'new-call', 'name': 'Read', 'arguments': {'path': 'fixture'}})
                    return {'turn': {'id': 'new-turn'}}
                return await super().rpc(method, params)

        old_call = {'role': 'assistant', 'tool_calls': [{'id': 'old', 'type': 'function',
                    'function': {'name': 'Read', 'arguments': '{"path":"fixture"}'}}]}
        messages = [{'role': 'user', 'content': 'read'}, old_call,
                    {'role': 'tool', 'tool_call_id': 'old', 'content': 'OLD'},
                    {'role': 'assistant', 'content': 'OLD'},
                    {'role': 'user', 'content': 'I changed the file; read again'}]
        app = ReadPeer()
        bridge = Bridge(app, response_timeout=.01)
        response = await bridge.complete('w', body(messages))
        self.assertEqual(response['choices'][0]['message']['tool_calls'][0]['id'], 'new-call')
        self.assertFalse(app.sent)
        await bridge.close()

    async def test_concurrent_admission_reserves_capacity_during_eviction(self):
        class SlowRelease(Peer):
            def __init__(self):
                super().__init__()
                self.releasing, self.proceed = asyncio.Event(), asyncio.Event()

            async def rpc(self, method, params):
                if method == 'thread/unsubscribe':
                    self.releasing.set()
                    await self.proceed.wait()
                return await super().rpc(method, params)

        app = SlowRelease()
        bridge = Bridge(app, max_sessions=1, response_timeout=.01)
        await bridge.complete('old', body())
        app.finish = False
        a = asyncio.create_task(bridge.complete('a', body()))
        await app.releasing.wait()
        b = asyncio.create_task(bridge.complete('b', body()))
        await asyncio.sleep(.02)
        app.proceed.set()
        await asyncio.gather(a, b, return_exceptions=True)
        self.assertLessEqual(len(bridge.states), 1)
        await bridge.close()

    async def test_threads_are_ephemeral_and_planner_reset_releases_previous(self):
        app = Peer()
        bridge = Bridge(app)
        await bridge.complete('planner', body())
        old = bridge.states['planner'].thread
        await bridge.complete('planner', body([{'role': 'user', 'content': 'new plan'}]))
        self.assertNotEqual(old, bridge.states['planner'].thread)
        starts = [p for m, p in app.calls if m == 'thread/start']
        self.assertTrue(all(p['ephemeral'] is True for p in starts))
        self.assertIn(('thread/unsubscribe', {'threadId': old}), app.calls)

    async def test_catalog_or_model_change_rejects_without_destroying_session(self):
        app = Peer()
        bridge = Bridge(app)
        first = await bridge.complete('w', body())
        state = bridge.states['w']
        messages = body()['messages'] + [first['choices'][0]['message'], {'role': 'user', 'content': 'next'}]
        for field, value in [('model', 'other'), ('tools', []), ('messages', [{'role': 'system', 'content': 'changed'}] + messages)]:
            changed = body(messages)
            changed[field] = value
            with self.assertRaises(ProtocolError):
                await bridge.complete('w', changed)
            self.assertIs(bridge.states.get('w'), state)

    async def test_response_timeout_retry_joins_same_inference(self):
        app = Peer()
        app.finish = False
        bridge = Bridge(app, response_timeout=.01)
        request = body()
        with self.assertRaises(ServiceUnavailable):
            await bridge.complete('w', request)
        state = bridge.states['w']
        await state.events.put({'kind': 'text', 'text': 'late answer'})
        answer = await bridge.complete('w', request)
        self.assertEqual(answer['choices'][0]['message']['content'], 'late answer')
        self.assertEqual(sum(m == 'turn/start' for m, _ in app.calls), 1)
        self.assertFalse(bridge.locks)

    async def test_idle_eviction_and_lock_cleanup_allow_transcript_restore(self):
        app = Peer()
        bridge = Bridge(app, idle_ttl=1, max_sessions=2)
        first = await bridge.complete('w', body())
        old = bridge.states['w'].thread
        bridge.states['w'].last_touch -= 2
        await bridge.reap()
        self.assertFalse(bridge.states)
        self.assertFalse(bridge.locks)
        self.assertFalse(app.sessions)
        messages = body()['messages'] + [first['choices'][0]['message'], {'role': 'user', 'content': 'next'}]
        await bridge.complete('w', body(messages))
        self.assertNotEqual(old, bridge.states['w'].thread)
        prompt = [p for m, p in app.calls if m == 'turn/start'][-1]['input'][0]['text']
        self.assertIn('answer', prompt)
        self.assertIn('next', prompt)

    async def test_abandoned_request_is_interrupted_and_released(self):
        app = Peer()
        app.finish = False
        bridge = Bridge(app, response_timeout=.01, idle_ttl=1)
        with self.assertRaises(ServiceUnavailable):
            await bridge.complete('w', body())
        bridge.states['w'].last_touch -= 2
        await bridge.reap()
        self.assertFalse(bridge.states)
        self.assertFalse(bridge.locks)
        self.assertFalse(app.sessions)
        self.assertEqual(sum(m == 'turn/interrupt' for m, _ in app.calls), 1)

    async def test_failed_turn_restores_history_and_reuses_completed_tool_result(self):
        class ToolPeer(Peer):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    self.calls.append((method, params))
                    state = self.sessions[params['threadId']]
                    state.pending['call-' + state.thread] = len(self.calls)
                    await state.events.put({'kind': 'tool', 'id': 'call-' + state.thread,
                                            'name': 'Read', 'arguments': {'path': 'fixture'}})
                    return {'turn': {'id': 'turn-' + state.thread}}
                return await super().rpc(method, params)

            async def send(self, message):
                await super().send(message)
                state = next(iter(self.sessions.values()))
                if len(self.sent) == 1:
                    await state.events.put({'kind': 'error', 'error': 'temporary upstream error', 'retryable': True})
                else:
                    await state.events.put({'kind': 'text', 'text': 'recovered'})

        app = ToolPeer()
        bridge = Bridge(app)
        request = body()
        first = await bridge.complete('w', request)
        msg = first['choices'][0]['message']
        result = {'role': 'tool', 'tool_call_id': msg['tool_calls'][0]['id'], 'content': 'random-marker-42'}
        request = body(request['messages'] + [msg, result])
        with self.assertRaises(ServiceUnavailable):
            await bridge.complete('w', request)
        recovered = await bridge.complete('w', request)
        self.assertEqual(recovered['choices'][0]['message']['content'], 'recovered')
        self.assertNotIn('tool_calls', recovered['choices'][0]['message'])
        self.assertEqual(len(app.sent), 2)
        self.assertEqual(app.sent[-1]['result']['contentItems'][0]['text'],
                         'Recovered recorded output (not a fresh execution):\nrandom-marker-42')
        prompt = [p for m, p in app.calls if m == 'turn/start'][-1]['input'][0]['text']
        self.assertIn('random-marker-42', prompt)

    async def test_restore_refuses_unresolved_or_duplicate_tool_results(self):
        msg = {'role': 'assistant', 'tool_calls': [{'id': 'c1', 'type': 'function',
                'function': {'name': 'Read', 'arguments': '{}'}}]}
        result = {'role': 'tool', 'tool_call_id': 'c1', 'content': 'done'}
        for suffix in ([msg], [msg, result, result]):
            app = Peer()
            bridge = Bridge(app)
            with self.assertRaises(SessionConflict):
                await bridge.complete('w', body(body()['messages'] + suffix))
            self.assertFalse(app.calls)
            self.assertFalse(bridge.locks)

    async def test_partial_parallel_results_and_fabricated_assistant_are_rejected(self):
        class ToolsPeer(Peer):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    self.calls.append((method, params))
                    state = self.sessions[params['threadId']]
                    for n in (1, 2):
                        state.pending[f'c{n}'] = n
                        await state.events.put({'kind': 'tool', 'id': f'c{n}', 'name': 'Read', 'arguments': {'n': n}})
                    return {'turn': {'id': 't'}}
                return await super().rpc(method, params)
        app = ToolsPeer()
        bridge = Bridge(app, response_timeout=.01)
        first = await bridge.complete('w', body())
        messages = body()['messages'] + [first['choices'][0]['message']]
        for delta in ([{'role': 'tool', 'tool_call_id': 'c1', 'content': 'one'}],
                      [{'role': 'assistant', 'content': 'fabricated'}]):
            with self.assertRaises(SessionConflict):
                await bridge.complete('w', body(messages + delta))
            self.assertFalse(app.sent)
            self.assertFalse(bridge.states['w'].broken)
        await bridge.close()

    async def test_parallel_tool_results_delivered_once_and_correlated(self):
        class ParallelPeer(Peer):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    self.calls.append((method, params))
                    state = self.sessions[params['threadId']]
                    for n in (1, 2):
                        state.pending[f'call-{n}'] = n + 10
                        await state.events.put({'kind': 'tool', 'id': f'call-{n}', 'name': 'Read', 'arguments': {'n': n}})
                    return {'turn': {'id': 't'}}
                return await super().rpc(method, params)

            async def send(self, message):
                await super().send(message)
                if len(self.sent) == 2:
                    await next(iter(self.sessions.values())).events.put({'kind': 'text', 'text': 'both done'})

        app = ParallelPeer()
        bridge = Bridge(app)
        request = body()
        response = await bridge.complete('w', request)
        msg = response['choices'][0]['message']
        self.assertEqual(len(msg['tool_calls']), 2)
        results = [{'role': 'tool', 'tool_call_id': f'call-{n}', 'content': str(n)} for n in (2, 1)]
        request = body(request['messages'] + [msg] + results)
        final = await bridge.complete('w', request)
        self.assertEqual(final, await bridge.complete('w', request))
        self.assertEqual([(m['id'], m['result']['contentItems'][0]['text']) for m in app.sent], [(12, '2'), (11, '1')])

    async def test_capacity_stays_bounded_without_evicting_active_work(self):
        app = Peer()
        app.finish = False
        bridge = Bridge(app, max_sessions=1, response_timeout=.01)
        with self.assertRaises(ServiceUnavailable):
            await bridge.complete('a', body())
        with self.assertRaises(ServiceUnavailable):
            await bridge.complete('b', body())
        self.assertEqual(list(bridge.states), ['a'])
        self.assertFalse(bridge.locks)
        await bridge.close()


if __name__ == '__main__':
    unittest.main()
