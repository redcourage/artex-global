"""Exercise real Norma -> HTTP -> Bridge using a deterministic Codex peer.

Run from the repository root. Requires Go and loopback sockets, no credentials,
external model calls, DB, or access to the running ARTEX service.
"""
import asyncio
from pathlib import Path

from bridge import serve_client
from sessions import Bridge, SUMMARY_SYSTEM
from test_compaction import ToolPeer


class IntegrationPeer(ToolPeer):
    async def rpc(self, method, params):
        if method == 'turn/start':
            state = self.sessions[params['threadId']]
            if state.system == SUMMARY_SYSTEM:
                self.calls.append((method, params))
                await state.events.put({'kind': 'text', 'text': '<summary>Fixture state preserved.</summary>'})
                return {'turn': {'id': 'summary'}}
        return await super().rpc(method, params)

    async def send(self, message):
        # Several independent worker/summary threads share the peer. Resolve the
        # pending RPC by id, just as app-server does, instead of picking a thread.
        self.sent.append(message)
        state = next(s for s in self.sessions.values() if message['id'] in s.pending.values())
        await self.emit_tool(state)


async def main():
    app = IntegrationPeer()
    bridge = Bridge(app)
    server = await asyncio.start_server(lambda r, w: serve_client(r, w, bridge, 'local-test'),
                                        '127.0.0.1', 0)
    process = None
    try:
        url = f'http://127.0.0.1:{server.sockets[0].getsockname()[1]}/v1'
        fixture = Path(__file__).resolve().parent / 'testdata/norma_compaction.go'
        process = await asyncio.create_subprocess_exec('go', 'run', str(fixture), url)
        if await asyncio.wait_for(process.wait(), 90) != 0:
            raise RuntimeError('Norma integration client failed')
        events = [event for event, _ in app.logs]
        assert 'history_compacted' in events, 'micro compaction was not exercised'
        assert 'summary_request' in events, 'summary lane was not exercised'
        assert 'history_rebased' in events, 'summary continuation was not exercised'
        assert 'history_conflict' not in events
        assert len(app.sent) == len(set(m['id'] for m in app.sent)), 'duplicate tool delivery'
        print('PASS: micro compaction, text-only summary isolation, full rebase, unique tool delivery')
    finally:
        if process is not None and process.returncode is None:
            process.terminate()
            await process.wait()
        server.close()
        await server.wait_closed()
        await bridge.close()


if __name__ == '__main__':
    asyncio.run(main())
