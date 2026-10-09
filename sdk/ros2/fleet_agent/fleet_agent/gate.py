"""TwistGate: the last check between an operator's twist and the robot's wheels.

No rclpy and no sockets: the gate is handed a ``publish(x, y, wz)`` function and
a clock, so every rule here is unit tested without a ROS graph (test/test_gate.py).

The rules (docs/DESIGN.md D3, "physics never waits on the network"):

- A setpoint is published only when it comes from the operator, bears the lease
  the gate was told is current, and after it is clamped to the manifest's drive
  limits. Anything else is dropped.
- Every stop the SDK decides (its own deadman, a revoked lease, a lost link) is
  published as zero velocity at once.
- **The watchdog.** ``tick()`` runs on a ROS timer, on the ROS thread. If the
  last published setpoint was non-zero and no valid twist has arrived for the
  deadman window, it publishes zero. It reads nothing but this object and a
  monotonic clock, so it holds when the SDK's own deadman cannot fire: the
  asyncio loop is blocked, or the link thread has died.
- A stop is repeated a few times, then the gate goes silent. The repeats cover a
  subscriber that drops one sample; the silence lets a twist mux fall back to its
  lower-priority inputs once teleop is over.

Two threads call in (the SDK's link thread and the ROS thread) and each owns a
timer, so one stalled thread never leaves the robot moving. The lock is held for
a publish and nothing else; if the watchdog cannot get it within one tick it
publishes zero anyway.
"""

from __future__ import annotations

import threading
import time
from typing import Callable, Optional

from .conversions import clamp_twist

__all__ = ["TwistGate"]

Publish = Callable[[float, float, float], None]


class TwistGate:
    #: How often the node calls ``tick()``.
    TICK_S = 0.02
    #: The watchdog fires this much before the deadline, so that with one tick of
    #: timer granularity and delivery time, zero is on the topic inside the window.
    EARLY_S = 0.05
    #: A stop is published once at once, then this many more times, this far apart.
    STOP_REPEATS = 5
    STOP_REPEAT_S = 0.05

    def __init__(
        self,
        publish: Publish,
        *,
        max_v_mps: float,
        max_w_radps: float,
        deadman_s: float,
        on_stop: Callable[[str], None] = lambda reason: None,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        """
        Args:
            publish: sends one velocity setpoint (linear x, linear y, angular z).
            max_v_mps, max_w_radps: the manifest's drive limits; setpoints are clamped to them.
            deadman_s: zero velocity is published within this long of the last valid twist.
            on_stop: called with the reason each time the robot is stopped while it was moving.
            clock: monotonic seconds. Never the ROS clock: sim time can pause, the deadman cannot.
        """
        if not deadman_s > self.EARLY_S + self.TICK_S:
            raise ValueError(f"deadman must be longer than {self.EARLY_S + self.TICK_S} s, got {deadman_s!r}")
        self._publish = publish
        self._max_v = max_v_mps
        self._max_w = max_w_radps
        self._deadman_s = deadman_s
        self._on_stop = on_stop
        self._clock = clock
        self._lock = threading.Lock()
        self._lease_id: Optional[str] = None
        self._closed = False
        self._moving = False
        self._last_valid = 0.0
        self._repeats_left = 0
        self._next_repeat = 0.0
        #: Stops the watchdog issued (the SDK's deadman did not get there first).
        self.watchdog_stops = 0

    @property
    def moving(self) -> bool:
        """True while the last published setpoint was non-zero."""
        return self._moving

    def set_lease(self, lease_id: Optional[str]) -> None:
        """Names the lease whose twist may be published; None stops and refuses all twist."""
        with self._lock:
            if lease_id != self._lease_id and self._moving:
                # A new driver, or none: the old driver's setpoint must not carry over.
                self._stop_locked("lease changed")
            self._lease_id = lease_id

    def operator_twist(self, lease_id: Optional[str], x: float, y: float, wz: float) -> bool:
        """Publishes one operator setpoint, clamped. Returns False if it was refused."""
        with self._lock:
            if self._closed or lease_id is None or lease_id != self._lease_id:
                return False
            x, y, wz = clamp_twist(x, y, wz, max_v_mps=self._max_v, max_w_radps=self._max_w)
            # Stamped before the publish: a publish that is slow to return must look
            # old to the watchdog, not fresh.
            self._last_valid = self._clock()
            self._moving = (x, y, wz) != (0.0, 0.0, 0.0)
            # A zero from the operator (keys released) is a stop like any other: repeat it.
            self._repeats_left = 0 if self._moving else self.STOP_REPEATS
            self._next_repeat = self._last_valid + self.STOP_REPEAT_S
            self._publish(x, y, wz)
            return True

    def stop(self, reason: str) -> None:
        """Publishes zero velocity now. Safe from any thread, and when already stopped."""
        with self._lock:
            self._stop_locked(reason)

    def close(self, reason: str) -> None:
        """Publishes zero velocity and refuses every twist from now on (the node is going away)."""
        with self._lock:
            self._closed = True
            self._stop_locked(reason)

    def tick(self) -> None:
        """The watchdog. Call every ``TICK_S`` from a timer that does not depend on the link."""
        if not self._lock.acquire(timeout=self.TICK_S):
            # The other thread is stuck inside a publish. Do not wait for it.
            self._publish(0.0, 0.0, 0.0)
            return
        try:
            now = self._clock()
            if self._moving:
                if now - self._last_valid >= self._deadman_s - self.EARLY_S:
                    self.watchdog_stops += 1
                    self._stop_locked("deadman: no valid twist (ROS watchdog)")
            elif self._repeats_left > 0 and now >= self._next_repeat:
                self._repeats_left -= 1
                self._next_repeat = now + self.STOP_REPEAT_S
                self._publish(0.0, 0.0, 0.0)
        finally:
            self._lock.release()

    def _stop_locked(self, reason: str) -> None:
        was_moving, self._moving = self._moving, False
        if not was_moving and self._lease_id is None:
            return  # nothing was ever commanded under a lease: stay off the topic
        self._publish(0.0, 0.0, 0.0)
        self._repeats_left = self.STOP_REPEATS
        self._next_repeat = self._clock() + self.STOP_REPEAT_S
        if was_moving:
            self._on_stop(reason)
