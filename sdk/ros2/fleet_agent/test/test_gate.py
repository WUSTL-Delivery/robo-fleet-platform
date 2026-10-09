"""TwistGate rules, with a fake clock and a list for a publisher. No ROS graph."""

import threading

import pytest
from fleet_agent.gate import TwistGate

ZERO = (0.0, 0.0, 0.0)
DEADMAN_S = 0.3

class Rig:
    def __init__(self, **kwargs):
        self.now = 100.0
        self.out = []
        self.stops = []
        args = dict(max_v_mps=1.0, max_w_radps=1.5, deadman_s=DEADMAN_S)
        args.update(kwargs)
        self.gate = TwistGate(
            lambda x, y, wz: self.out.append((x, y, wz)),
            on_stop=self.stops.append,
            clock=lambda: self.now,
            **args,
        )

    def advance(self, seconds, tick=True):
        """Moves the clock in watchdog ticks, calling tick() the way the node's timer does."""
        end = self.now + seconds
        while self.now + TwistGate.TICK_S <= end + 1e-9:
            self.now += TwistGate.TICK_S
            if tick:
                self.gate.tick()
        self.now = end


def test_twist_is_published_only_under_the_current_lease():
    rig = Rig()
    assert rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.2) is False  # no lease granted yet
    rig.gate.set_lease("ls_a")
    assert rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.2) is True
    assert rig.gate.operator_twist("ls_b", 0.9, 0.0, 0.0) is False  # someone else's lease
    assert rig.gate.operator_twist(None, 0.9, 0.0, 0.0) is False
    assert rig.out == [(0.5, 0.0, 0.2)]
    assert rig.gate.moving


def test_twist_is_clamped_to_the_drive_limits():
    rig = Rig()
    rig.gate.set_lease("ls_a")
    rig.gate.operator_twist("ls_a", 5.0, 0.0, -9.0)
    rig.gate.operator_twist("ls_a", float("nan"), 0.0, 0.1)
    assert rig.out == [(1.0, 0.0, -1.5), ZERO]
    assert not rig.gate.moving  # a non-finite setpoint is a stop


def test_watchdog_zeroes_inside_the_deadman_window_and_not_before():
    rig = Rig()
    rig.gate.set_lease("ls_a")
    rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.0)
    rig.advance(0.25)
    assert rig.out == [(0.5, 0.0, 0.0)], "stopped early"
    rig.advance(0.05)
    assert rig.out[-1] == ZERO, "not stopped by the deadline"
    assert rig.gate.watchdog_stops == 1 and not rig.gate.moving
    assert rig.stops == ["deadman: no valid twist (ROS watchdog)"]


def test_a_steady_twist_stream_is_never_interrupted():
    rig = Rig()
    rig.gate.set_lease("ls_a")
    for _ in range(50):  # 10 Hz, the console's rate, for 5 s
        rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.0)
        rig.advance(0.1)
    assert ZERO not in rig.out and rig.gate.watchdog_stops == 0


def test_a_stop_is_repeated_then_the_gate_goes_silent():
    rig = Rig()
    rig.gate.set_lease("ls_a")
    rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.0)
    rig.advance(5.0)
    # Once at the stop, then the repeats; nothing after, so a twist mux can fall through.
    assert rig.out == [(0.5, 0.0, 0.0)] + [ZERO] * (1 + TwistGate.STOP_REPEATS)
    assert rig.gate.watchdog_stops == 1


def test_a_new_twist_after_a_stop_drives_again_and_rearms():
    rig = Rig()
    rig.gate.set_lease("ls_a")
    rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.0)
    rig.advance(0.3)  # stopped; repeats still pending
    rig.gate.operator_twist("ls_a", 0.4, 0.0, 0.0)
    rig.advance(0.2)
    assert rig.out[-1] == (0.4, 0.0, 0.0), "a stale stop repeat overrode a fresh twist"
    rig.advance(0.2)
    assert rig.out[-1] == ZERO and rig.gate.watchdog_stops == 2


def test_an_operator_zero_is_a_stop_that_needs_no_watchdog():
    rig = Rig()
    rig.gate.set_lease("ls_a")
    rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.0)
    rig.gate.operator_twist("ls_a", 0.0, 0.0, 0.0)
    rig.advance(1.0)
    assert rig.out == [(0.5, 0.0, 0.0)] + [ZERO] * (1 + TwistGate.STOP_REPEATS)
    assert rig.gate.watchdog_stops == 0 and rig.stops == []


def test_sdk_stop_zeroes_at_once_and_reports_why():
    rig = Rig()
    rig.gate.set_lease("ls_a")
    rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.0)
    rig.gate.stop("lease revoked")
    assert rig.out[-1] == ZERO and rig.stops == ["lease revoked"]
    rig.gate.set_lease(None)
    assert rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.0) is False  # the revoked lease is dead
    rig.advance(1.0)
    assert rig.gate.watchdog_stops == 0 and rig.out[-1] == ZERO


def test_losing_or_changing_the_lease_while_moving_zeroes():
    for new_lease in (None, "ls_b"):
        rig = Rig()
        rig.gate.set_lease("ls_a")
        rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.0)
        rig.gate.set_lease(new_lease)
        assert rig.out[-1] == ZERO and not rig.gate.moving


def test_close_zeroes_and_refuses_everything_after():
    rig = Rig()
    rig.gate.set_lease("ls_a")
    rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.0)
    rig.gate.close("fleet_agent is shutting down")
    assert rig.out[-1] == ZERO and rig.stops == ["fleet_agent is shutting down"]
    assert rig.gate.operator_twist("ls_a", 0.5, 0.0, 0.0) is False
    assert rig.out[-1] == ZERO and not rig.gate.moving


def test_without_a_lease_the_gate_stays_off_the_topic():
    rig = Rig()
    rig.gate.stop("link to fleet-server lost")
    rig.advance(1.0)
    assert rig.out == [] and rig.stops == []


def test_watchdog_publishes_zero_when_the_other_thread_is_stuck_in_a_publish():
    release = threading.Event()
    entered = threading.Event()
    out = []

    def publish(x, y, wz):
        if (x, y, wz) != ZERO:
            entered.set()
            release.wait(5)  # the link thread hangs inside its publish, holding the lock
        out.append((x, y, wz))

    gate = TwistGate(publish, max_v_mps=1.0, max_w_radps=1.5, deadman_s=DEADMAN_S)
    gate.set_lease("ls_a")
    stuck = threading.Thread(target=gate.operator_twist, args=("ls_a", 0.5, 0.0, 0.0))
    stuck.start()
    try:
        assert entered.wait(5)
        gate.tick()
        assert out == [ZERO]
    finally:
        release.set()
        stuck.join(5)


def test_a_deadman_shorter_than_the_watchdog_can_keep_is_refused():
    with pytest.raises(ValueError):
        Rig(deadman_s=0.04)
