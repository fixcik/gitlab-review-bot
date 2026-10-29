"""Server-Sent Events broker: GET /events streams "order.updated"."""

import asyncio
import json


class Broker:
    def __init__(self) -> None:
        self._queues: set[asyncio.Queue] = set()

    def publish(self, event: str, data: dict) -> None:
        msg = f"event: {event}\ndata: {json.dumps(data)}\n\n"
        for q in list(self._queues):
            q.put_nowait(msg)

    async def stream(self):
        q: asyncio.Queue = asyncio.Queue()
        self._queues.add(q)
        try:
            while True:
                yield await q.get()
        finally:
            self._queues.discard(q)
