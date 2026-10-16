"""Experimental local, non-streaming ARTEX -> Codex subscription bridge.

Codex proposes tools; ARTEX executes them. This probe is not a general API server.
Completed tool results are retained during recovery; ambiguous results are rejected.
"""
import argparse
import asyncio
import base64
import hmac
import json
import os
from pathlib import Path


from protocol import ProtocolError, ServiceUnavailable, SessionConflict
from app_server import AppServer


from sessions import (Bridge, Session, UsageTracker, added_messages, dynamic_tools,
                      completion, pending_results)


async def serve_client(reader, writer, bridge, token):
    status, payload = 200, {}
    try:
        header = await asyncio.wait_for(reader.readuntil(b'\r\n\r\n'), 10)
        lines = header.decode().split('\r\n')
        method, path, _ = lines[0].split()
        headers = {k.lower(): v.strip() for k,v in (line.split(':', 1) for line in lines[1:] if line)}
        if method == 'GET' and path == '/health':
            healthy = bridge.app.is_healthy()
            status, payload = (200 if healthy else 503), {'ok': healthy}
        elif not hmac.compare_digest(headers.get('authorization', ''), 'Bearer ' + token):
            status, payload = 401, {'error': {'message': 'Unauthorized'}}
        elif method == 'GET' and path == '/v1/models':
            payload = await bridge.app.models()
        elif method != 'POST' or path != '/v1/chat/completions':
            status, payload = 404, {'error': {'message': 'Unsupported endpoint'}}
        else:
            length = int(headers.get('content-length', '0'))
            if not 0 < length <= 8*1024*1024:
                raise ProtocolError('Invalid request size')
            body = json.loads(await asyncio.wait_for(reader.readexactly(length), 15))
            task = asyncio.create_task(bridge.complete(headers.get('x-artex-codex-session', ''), body))
            disconnected = asyncio.create_task(reader.read(1))
            try:
                done, _ = await asyncio.wait({task, disconnected}, timeout=180,
                                             return_when=asyncio.FIRST_COMPLETED)
                if task not in done:
                    task.cancel()
                    await asyncio.gather(task, return_exceptions=True)
                    raise ServiceUnavailable('Client disconnected or request timed out; retry identical request')
                payload = task.result()
            finally:
                disconnected.cancel()
                await asyncio.gather(disconnected, return_exceptions=True)
    except Exception as exc:
        status = getattr(exc, 'status', 400 if isinstance(exc, (ValueError, asyncio.IncompleteReadError)) else 500)
        message = str(exc) if isinstance(exc, ProtocolError) else ('Invalid HTTP request' if status == 400 else 'Bridge internal error')
        bridge.app.log('http_error', status=status, reason=type(exc).__name__)
        payload = {'error': {'message': message}}
        if hasattr(exc, 'artex_usage'):
            payload['artex_usage'] = exc.artex_usage
    try:
        data = json.dumps(payload, ensure_ascii=False).encode()
        metering = ''
        if 'artex_usage' in payload:
            value = base64.b64encode(json.dumps(payload['artex_usage']).encode()).decode()
            metering = f'X-Artex-Usage: {value}\r\n'
        retry = 'Retry-After: 1\r\n' if status == 503 else ''
        writer.write(f'HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {len(data)}\r\n{metering}{retry}Connection: close\r\n\r\n'.encode() + data)
        await writer.drain()
    finally:
        writer.close()


async def main(args):
    token = os.environ.get('ARTEX_BRIDGE_TOKEN', '')
    if not token:
        raise SystemExit('Set ARTEX_BRIDGE_TOKEN to a local probe token (not an OpenAI key)')
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    app = AppServer(output)
    bridge = None
    maintenance = None
    try:
        await app.start()
        bridge = Bridge(app, idle_ttl=args.idle_ttl, max_sessions=args.max_sessions,
                        response_timeout=args.response_timeout)
        maintenance = asyncio.create_task(bridge.maintain())
        server = await asyncio.start_server(lambda r,w: serve_client(r,w,bridge,token),
                                            '127.0.0.1', args.port, limit=8*1024*1024)
        app.log('listening', port=server.sockets[0].getsockname()[1])
        async with server:
            await server.serve_forever()
    finally:
        if maintenance:
            maintenance.cancel()
            await asyncio.gather(maintenance, return_exceptions=True)
        if bridge:
            await bridge.close()
        await app.close()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--port', type=int, default=18789)
    parser.add_argument('--output', required=True)
    parser.add_argument('--idle-ttl', type=float, default=1800, help='Seconds before idle or abandoned sessions are released')
    parser.add_argument('--max-sessions', type=int, default=256)
    parser.add_argument('--response-timeout', type=float, default=150, help='HTTP wait before returning retryable 503')
    args = parser.parse_args()
    if args.idle_ttl <= 0 or args.max_sessions <= 0 or not 0 < args.response_timeout < 180:
        parser.error('TTL and capacity must be positive; response timeout must be between 0 and 180 seconds')
    try:
        asyncio.run(main(args))
    except KeyboardInterrupt:
        pass
