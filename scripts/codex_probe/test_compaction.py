"""Norma context maintenance must not invalidate a live Codex session."""
import copy
import unittest

from sessions import Bridge, added_messages
from protocol import ProtocolError, ServiceUnavailable, SessionConflict
from test_recovery import Peer, body

CLEARED = '[Old tool result content cleared]'
PREAMBLE = ('This session is being continued from a previous conversation that ran out of context. '
            'The conversation is summarized below:\n\n')


class ToolPeer(Peer):
    def __init__(self):
        super().__init__()
        self.number = 0
        self.logs = []

    def log(self, event, **fields):
        self.logs.append((event, fields))

    async def emit_tool(self, state):
        self.number += 1
        cid = f'read-{self.number}'
        state.pending[cid] = self.number
        await state.events.put({'kind': 'tool', 'id': cid, 'name': 'Read',
                                'arguments': {'n': self.number}})

    async def rpc(self, method, params):
        if method == 'turn/start':
            self.calls.append((method, params))
            state = self.sessions[params['threadId']]
            prompt = params['input'][0]['text']
            if 'Continue the existing ARTEX conversation' in prompt:
                await state.events.put({'kind': 'text', 'text': 'restored'})
            else:
                await self.emit_tool(state)
            return {'turn': {'id': 'turn'}}
        return await super().rpc(method, params)

    async def send(self, message):
        await super().send(message)
        state = next(iter(self.sessions.values()))
        await self.emit_tool(state)


class CompactionTests(unittest.IsolatedAsyncioTestCase):
    async def setup_chain(self, rounds=8):
        app = ToolPeer()
        bridge = Bridge(app)
        self.addAsyncCleanup(bridge.close)
        request = body()
        for _ in range(rounds):
            response = await bridge.complete('w', request)
            message = response['choices'][0]['message']
            request['messages'] += [message, {'role': 'tool',
                'tool_call_id': message['tool_calls'][0]['id'], 'content': f'output-{app.number}'}]
        return app, bridge, request

    async def test_micro_compaction_and_resume_keep_thread_and_deliver_once(self):
        app, bridge, request = await self.setup_chain()
        thread = bridge.states['w'].thread
        original = copy.deepcopy(request)
        for m in request['messages'][0:-10]:
            if m['role'] == 'tool':
                m['content'] = CLEARED
        response = await bridge.complete('w', request)
        self.assertEqual(await bridge.complete('w', request), response)
        self.assertEqual(bridge.states['w'].thread, thread)
        self.assertEqual([m['id'] for m in app.sent], list(range(1, 9)))
        # Resume reads the original transcript, not the transient compacted view.
        original['messages'] += [response['choices'][0]['message'],
            {'role': 'tool', 'tool_call_id': 'read-9', 'content': 'output-9'}]
        await bridge.complete('w', original)
        self.assertEqual(bridge.states['w'].thread, thread)
        self.assertTrue(any(e == 'history_compacted' for e, _ in app.logs))

    async def test_full_compaction_restores_tail_and_caches_retry(self):
        app, bridge, request = await self.setup_chain()
        state = bridge.states['w']
        old_thread = state.thread
        state.usage.update({'inputTokens': 100, 'cachedInputTokens': 20,
                            'outputTokens': 10, 'reasoningOutputTokens': 0})
        compacted = body([{'role': 'user', 'content': PREAMBLE + 'Summary: completed reads.'}]
                         + request['messages'][-4:])
        answer = await bridge.complete('w', compacted)
        self.assertNotEqual(bridge.states['w'].thread, old_thread)
        self.assertEqual(answer, await bridge.complete('w', compacted))
        self.assertEqual(answer['artex_usage']['samples'][0]['input_tokens'], 100)
        self.assertTrue(any(method == 'turn/interrupt' for method, _ in app.calls))
        self.assertTrue(any(e == 'history_rebased' for e, _ in app.logs))
        prompt = [p for method, p in app.calls if method == 'turn/start'][-1]['input'][0]['text']
        self.assertIn('completed reads', prompt)
        self.assertIn('output-8', prompt)

    async def test_summary_isolated_from_worker_and_accepts_historical_tools(self):
        app, bridge, request = await self.setup_chain()
        state = bridge.states['w']
        thread, pending = state.thread, dict(state.pending)
        summary = {'model': 'test-model', 'messages': [
            {'role': 'system', 'content': 'You are a helpful AI assistant tasked with summarizing conversations.'},
            *request['messages'], {'role': 'user', 'content':
                'CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.\n\nSummarize.\n\n'
                'REMINDER: Do NOT call any tools. Respond with plain text only — an <analysis> block '
                'followed by a <summary> block. Tool calls will be rejected and you will fail the task.'}]}
        response = await bridge.complete('w', summary)
        self.assertEqual(response, await bridge.complete('w', summary))
        self.assertEqual(state.thread, thread)
        self.assertEqual(state.pending, pending)
        starts = [p for method, p in app.calls if method == 'thread/start']
        self.assertEqual(len(starts), 2)
        self.assertEqual(starts[-1]['dynamicTools'], [])
        self.assertEqual(len(app.sent), 7)

    async def test_summary_cannot_hide_unresolved_or_changed_pending_tool(self):
        app, bridge, request = await self.setup_chain()
        old_calls = list(app.calls)
        invalid = [body([{'role': 'user', 'content': PREAMBLE + 'all done'}]),
                   body([{'role': 'user', 'content': PREAMBLE + 'summary'}] + request['messages'][-4:-1])]
        changed = body([{'role': 'user', 'content': PREAMBLE + 'summary'}] + copy.deepcopy(request['messages'][-4:]))
        changed['messages'][-2]['tool_calls'][0]['function']['arguments'] = '{"n":999}'
        invalid.append(changed)
        for value in invalid:
            with self.subTest(value=len(value['messages'])):
                with self.assertRaises(SessionConflict):
                    await bridge.complete('w', value)
        self.assertEqual(app.calls, old_calls)
        self.assertFalse(bridge.states['w'].broken)

    async def test_unknown_history_change_still_fails_with_private_diagnostics(self):
        app, bridge, request = await self.setup_chain()
        request['messages'][2]['content'] = 'secret-altered-tool-output'
        with self.assertRaises(SessionConflict):
            await bridge.complete('w', request)
        logs = [fields for event, fields in app.logs if event == 'history_conflict']
        self.assertEqual(logs[-1]['session'], 'w')
        self.assertEqual(logs[-1]['first_difference'], 2)
        self.assertNotIn('secret-altered', str(logs))

    async def test_rebase_replays_omitted_completed_tool_without_exposing_it(self):
        app, bridge, request = await self.setup_chain()
        original_rpc = app.rpc
        original_send = app.send

        async def rpc(method, params):
            if method == 'turn/start':
                app.calls.append((method, params))
                state = app.sessions[params['threadId']]
                state.pending['recovered-read'] = 99
                await state.events.put({'kind': 'tool', 'id': 'recovered-read',
                                        'name': 'Read', 'arguments': {'n': 1}})
                return {'turn': {'id': 'recovered'}}
            return await original_rpc(method, params)

        async def send(message):
            if message['id'] == 99:
                app.sent.append(message)
                state = next(iter(app.sessions.values()))
                await state.events.put({'kind': 'text', 'text': 'no duplicate execution'})
            else:
                await original_send(message)

        app.rpc, app.send = rpc, send
        compacted = body([{'role': 'user', 'content': PREAMBLE + 'completed reads'}]
                         + request['messages'][-4:])
        response = await bridge.complete('w', compacted)
        self.assertEqual(response['choices'][0]['message']['content'], 'no duplicate execution')
        self.assertIn('output-1', app.sent[-1]['result']['contentItems'][0]['text'])
        self.assertIsNone(bridge.states['w'].rebase_replay)

    async def test_repeated_micro_compaction_does_not_grow_threads(self):
        app, bridge, request = await self.setup_chain()
        for _ in range(40):
            for message in request['messages'][:-10]:
                if message['role'] == 'tool':
                    message['content'] = CLEARED
            response = await bridge.complete('w', request)
            request['messages'] += [response['choices'][0]['message'],
                {'role': 'tool', 'tool_call_id': f'read-{app.number}', 'content': f'output-{app.number}'}]
        self.assertEqual(len(app.sent), 47)
        self.assertEqual(len(set(m['id'] for m in app.sent)), 47)
        self.assertEqual(sum(method == 'thread/start' for method, _ in app.calls), 1)

    async def test_rebase_can_retry_failed_thread_or_turn_start(self):
        for failing_method in ('thread/start', 'turn/start'):
            with self.subTest(method=failing_method):
                app, bridge, request = await self.setup_chain()
                original_rpc = app.rpc
                failed = False

                async def rpc(method, params):
                    nonlocal failed
                    if method == failing_method and not failed:
                        failed = True
                        raise ServiceUnavailable('temporary backend failure')
                    return await original_rpc(method, params)

                app.rpc = rpc
                compacted = body([{'role': 'user', 'content': PREAMBLE + 'completed reads'}]
                                 + request['messages'][-4:])
                with self.assertRaises(ServiceUnavailable):
                    await bridge.complete('w', compacted)
                response = await bridge.complete('w', compacted)
                self.assertEqual(response['choices'][0]['message']['content'], 'restored')
                self.assertEqual(len(app.sent), 7)

    async def test_parallel_new_results_cannot_be_replaced_by_cleared_markers(self):
        class ParallelPeer(ToolPeer):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    state = self.sessions[params['threadId']]
                    for _ in range(7):
                        await self.emit_tool(state)
                    return {'turn': {'id': 'parallel'}}
                return await super().rpc(method, params)

        app = ParallelPeer()
        bridge = Bridge(app)
        self.addAsyncCleanup(bridge.close)
        request = body()
        response = await bridge.complete('w', request)
        request['messages'].append(response['choices'][0]['message'])
        count = len(response['choices'][0]['message']['tool_calls'])
        request['messages'] += [{'role': 'tool', 'tool_call_id': f'read-{i}',
                                 'content': CLEARED if i <= 2 else f'output-{i}'} for i in range(1, count + 1)]
        with self.assertRaisesRegex(SessionConflict, 'unresolved tool result'):
            await bridge.complete('w', request)
        self.assertEqual(app.sent, [])
        self.assertFalse(bridge.states['w'].broken)

    async def test_parallel_calls_fit_norma_recent_result_budget(self):
        class ParallelPeer(ToolPeer):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    state = self.sessions[params['threadId']]
                    for _ in range(7):
                        await self.emit_tool(state)
                    return {'turn': {'id': 'parallel'}}
                return await super().rpc(method, params)

            async def send(self, message):
                self.sent.append(message)

        app = ParallelPeer()
        bridge = Bridge(app)
        self.addAsyncCleanup(bridge.close)
        request = body()
        response = await bridge.complete('w', request)
        calls = response['choices'][0]['message']['tool_calls']
        self.assertEqual(len(calls), 5)
        request['messages'] += [response['choices'][0]['message']] + [
            {'role': 'tool', 'tool_call_id': call['id'], 'content': 'done'} for call in calls]
        response = await bridge.complete('w', request)
        self.assertEqual(len(response['choices'][0]['message']['tool_calls']), 2)
        self.assertEqual(len(app.sent), 5)

    async def test_cold_start_then_transcript_resume_rehydrates_in_new_thread(self):
        app = ToolPeer()
        bridge = Bridge(app)
        self.addAsyncCleanup(bridge.close)
        request = body([{'role': 'user', 'content': 'original task'},
                        {'role': 'assistant', 'tool_calls': [{'id': 'r', 'type': 'function',
                         'function': {'name': 'Read', 'arguments': '{"n":1}'}}]},
                        {'role': 'tool', 'tool_call_id': 'r', 'content': CLEARED}])
        response = await bridge.complete('w', request)
        thread = bridge.states['w'].thread
        request['messages'][2]['content'] = 'original persisted output'
        request['messages'] += [response['choices'][0]['message'], {'role': 'user', 'content': 'continue'}]
        await bridge.complete('w', request)
        self.assertNotEqual(bridge.states['w'].thread, thread)
        self.assertEqual(app.sent, [])
        prompt = [p for method, p in app.calls if method == 'turn/start'][-1]['input'][0]['text']
        self.assertIn('original persisted output', prompt)

    def test_marker_does_not_allow_user_or_tool_arguments_to_change(self):
        for old, new in [([{'role': 'user', 'content': 'original'}],
                          [{'role': 'user', 'content': CLEARED}]),
                         ([{'role': 'tool', 'tool_call_id': 'missing', 'content': 'original'}],
                          [{'role': 'tool', 'tool_call_id': 'missing', 'content': CLEARED}])]:
            with self.assertRaises(ProtocolError):
                added_messages(old, new)


if __name__ == '__main__':
    unittest.main()
