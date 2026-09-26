"""A background asyncio loop, for running the SDK alongside a rclpy executor.

The Python SDK is asyncio and rclpy is not, so one of them has to live on another
thread. The SDK goes there, because its deadman (300 ms without a valid twist -> zero
velocity) is timed by ``loop.call_later`` and must not be delayed by a slow ROS
callback. Nothing in ``rclpy.spin`` can starve it.

Direction of travel, in both cases explicit:

- **ROS -> SDK** (help requests, shutdown): :meth:`AsyncioThread.submit`, which returns a
  ``concurrent.futures.Future``.
- **SDK -> ROS** (twist, lease changes): the SDK handler runs on this thread and hands
  work back through ``agent_node``'s own lock-guarded publish and its call-soon queue.
"""

from __future__ import annotations

import asyncio
import threading
from collections.abc import Coroutine
from concurrent.futures import Future
from typing import Any, TypeVar

__all__ = ["AsyncioThread"]

T = TypeVar("T")


class AsyncioThread:
    """Owns one event loop on a daemon thread, started and stopped explicitly."""

    def __init__(self, name: str = "fleet-agent-loop") -> None:
        """Create the thread; nothing runs until :meth:`start`."""
        self._name = name
        self._loop: asyncio.AbstractEventLoop | None = None
        self._thread: threading.Thread | None = None
        self._ready = threading.Event()

    @property
    def loop(self) -> asyncio.AbstractEventLoop:
        """The running loop. Raises RuntimeError before :meth:`start` returns."""
        if self._loop is None:
            raise RuntimeError("AsyncioThread.start() has not completed")
        return self._loop

    def start(self) -> None:
        """Start the loop and block until it is running."""
        if self._thread is not None:
            return
        self._thread = threading.Thread(target=self._run, name=self._name, daemon=True)
        self._thread.start()
        self._ready.wait()

    def _run(self) -> None:
        loop = asyncio.new_event_loop()
        asyncio.set_event_loop(loop)
        self._loop = loop
        self._ready.set()
        try:
            loop.run_forever()
        finally:
            # Let everything still pending settle before the loop closes, so a task
            # cancelled during shutdown does not surface as "never retrieved".
            try:
                pending = asyncio.all_tasks(loop)
                for task in pending:
                    task.cancel()
                if pending:
                    loop.run_until_complete(
                        asyncio.gather(*pending, return_exceptions=True)
                    )
                loop.run_until_complete(loop.shutdown_asyncgens())
            finally:
                loop.close()

    def submit(self, coro: Coroutine[Any, Any, T]) -> Future[T]:
        """Schedule a coroutine on the loop from any thread."""
        return asyncio.run_coroutine_threadsafe(coro, self.loop)

    def call_soon(self, fn: Any, *args: Any) -> None:
        """Schedule a plain callable on the loop from any thread."""
        self.loop.call_soon_threadsafe(fn, *args)

    def stop(self, timeout: float = 5.0) -> None:
        """Stop the loop and join the thread. Safe to call twice."""
        if self._loop is None or self._thread is None:
            return
        self._loop.call_soon_threadsafe(self._loop.stop)
        self._thread.join(timeout=timeout)
        self._thread = None
        self._loop = None
        self._ready.clear()
