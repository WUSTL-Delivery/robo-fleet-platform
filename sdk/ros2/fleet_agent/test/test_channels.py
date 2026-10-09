"""Launch test: channels as ROS topics, help as a service.

The node runs against a real fleet-server. The test plays both ends around it:
the robot's application node (a ``ChannelTap`` per channel, on plain ROS topics,
and a client of ``fleet/request_help``) and a service on the bus (``Peer``, the
place of a dispatcher). Nothing here touches a websocket on the robot's side,
which is the point of the bridge.

Covered: the manifest declares the channels; a message for the robot arrives on
the in topic and the reply published on the out topic arrives at the service as
channel.message; broadcasts both ways; acked sends answered by fleet_agent
(once per send, never when nothing listens) and passed through on a raw
channel; bad out messages; the Go example dispatcher end to end; and the help
service, accepted, refused and offline.
"""

import asyncio
import os
import subprocess
import sys
import time
import unittest

import launch
import launch_testing.actions
import pytest
import rclpy

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fleet_harness as harness  # noqa: E402

NAME = "channel-bot"
CHANNELS = ["jobs", "status", "rawjobs", "unheard"]
#: Long enough for an ack, or a second copy, to have arrived if one was coming.
QUIET_S = 0.7


@pytest.mark.launch_test
def generate_test_description():
    server = harness.FleetServer.start()
    agent = harness.agent_node(server, NAME, {"channels": CHANNELS, "raw_channels": ["rawjobs"]})
    return launch.LaunchDescription([agent, launch_testing.actions.ReadyToTest()]), {"server": server}


class TestChannels(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        rclpy.init()
        cls.rec = harness.Recorder("test_channels", "/test/channels/no_cmd_vel")
        # The application node's topics. "unheard" gets none until its test adds one.
        cls.taps = {name: harness.ChannelTap(cls.rec.node, name) for name in ("jobs", "status", "rawjobs")}

    @classmethod
    def tearDownClass(cls):
        cls.rec.close()
        rclpy.shutdown()

    def _run(self, server, body):
        """Runs ``body(peer, robot_id)`` with a service client on the bus and the taps matched."""

        async def scenario():
            async with harness.watching(server) as watcher, harness.peering(server) as peer:
                robot = await watcher.robot(NAME, lambda r: "channels" in (r.get("manifest") or {}))
                for tap in self.taps.values():
                    await tap.ready()
                return await body(peer, robot["robot_id"])

        return asyncio.run(scenario())

    def test_1_manifest_declares_the_channels(self, server):
        async def scenario():
            async with harness.watching(server) as watcher:
                return await watcher.robot(NAME, lambda r: r.get("manifest"))

        # No drive, no battery: the console shows neither, and broadcasts on these
        # channels reach the robot because it declares them.
        self.assertEqual(asyncio.run(scenario())["manifest"], {"channels": CHANNELS})

    def test_2_a_message_arrives_on_the_in_topic_and_the_reply_on_the_out_topic_reaches_the_sender(self, server):
        tap = self.taps["status"]

        async def body(peer, robot_id):
            start = time.monotonic()
            assignment = {"order": "o-17", "waypoints": [[38.6488, -90.3108], [38.6491, -90.3102]], "note": "café"}
            await peer.send("status", assignment, to=robot_id)
            sender, data, acked = await tap.next(since=start)
            self.assertEqual((sender, data, acked), (peer.id, assignment, False))

            # The application replies to whoever sent it, by publishing on the out topic.
            tap.send({"order": "o-17", "state": "accepted"}, to=sender)
            got = await peer.next(since=start)
            self.assertEqual(got, ("status", robot_id, {"order": "o-17", "state": "accepted"}))

            # `to` left empty broadcasts: every client subscribed to the channel gets it.
            await peer.listen("status")
            mark = time.monotonic()
            tap.send({"state": "en_route"})
            self.assertEqual(await peer.next(since=mark), ("status", robot_id, {"state": "en_route"}))

            # A broadcast from the service reaches the robot because its manifest declares the channel.
            mark = time.monotonic()
            await peer.send("status", {"announce": "curfew"})
            self.assertEqual(await tap.next(since=mark), (peer.id, {"announce": "curfew"}, False))

            # Data is any JSON value, not only an object, and crosses unchanged both ways.
            for value in ([1, "two", None, 2.5], "plain text", 42, True, None):
                mark = time.monotonic()
                await peer.send("status", value, to=robot_id)
                self.assertEqual(await tap.next(since=mark), (peer.id, value, False))
                tap.send(value, to=peer.id)
                self.assertEqual(await peer.next(since=mark), ("status", robot_id, value))

            await asyncio.sleep(QUIET_S)
            self.assertEqual(len(tap.since(start)), 7, tap.since(start))  # nothing arrived twice

        self._run(server, body)

    def test_3_an_acked_send_is_answered_by_fleet_agent_and_shown_once(self, server):
        tap = self.taps["jobs"]

        async def body(peer, robot_id):
            start = time.monotonic()
            seq = int(time.time() * 1000)
            job = {"id": "job-1", "task": "inspect"}
            await peer.send("jobs", {"seq": seq, "data": job}, to=robot_id)
            # The application sees the inner data, flagged as already answered...
            self.assertEqual(await tap.next(since=start), (peer.id, job, True))
            # ...and the sender gets the ack without the application doing anything.
            self.assertEqual(await peer.next(since=start), ("jobs", robot_id, {"ack": seq}))

            # A re-send (the sender lost the ack) is acked again and not shown again.
            mark = time.monotonic()
            await peer.send("jobs", {"seq": seq, "data": job}, to=robot_id)
            self.assertEqual(await peer.next(since=mark), ("jobs", robot_id, {"ack": seq}))
            # A new seq is a new send, even with the same data.
            await peer.send("jobs", {"seq": seq + 1, "data": job}, to=robot_id)
            await peer.next(lambda m: m[2] == {"ack": seq + 1}, since=mark)
            # Ordinary data on the same channel still passes, unflagged; so does an ack
            # addressed to the robot (the application may be the sender of an acked send).
            await peer.send("jobs", {"cancel": "job-1"}, to=robot_id)
            await peer.send("jobs", {"ack": 5}, to=robot_id)
            await tap.next(lambda m: m[1] == {"ack": 5}, since=mark)
            await asyncio.sleep(QUIET_S)
            self.assertEqual(
                tap.since(start),
                [(peer.id, job, True), (peer.id, job, True), (peer.id, {"cancel": "job-1"}, False), (peer.id, {"ack": 5}, False)],
            )

        self._run(server, body)

    def test_4_an_acked_send_is_not_answered_while_nothing_subscribes(self, server):
        async def body(peer, robot_id):
            start = time.monotonic()
            seq = int(time.time() * 1000)
            for _ in range(3):  # the sender re-sends; no application is listening on this channel
                await peer.send("unheard", {"seq": seq, "data": {"id": "job-9"}}, to=robot_id)
                await asyncio.sleep(0.3)
            self.assertEqual(peer.since(start), [], "fleet_agent acked a message nobody could receive")

            # The application comes up: the sender's next re-send is delivered and acked.
            tap = harness.ChannelTap(self.rec.node, "unheard")
            try:
                await tap.ready()
                mark = time.monotonic()
                await peer.send("unheard", {"seq": seq, "data": {"id": "job-9"}}, to=robot_id)
                self.assertEqual(await tap.next(since=mark), (peer.id, {"id": "job-9"}, True))
                self.assertEqual(await peer.next(since=mark), ("unheard", robot_id, {"ack": seq}))
            finally:
                tap.close()

        self._run(server, body)

    def test_5_a_raw_channel_passes_acked_sends_through_for_the_application_to_answer(self, server):
        tap = self.taps["rawjobs"]

        async def body(peer, robot_id):
            start = time.monotonic()
            message = {"seq": 7, "data": {"id": "job-2"}}
            await peer.send("rawjobs", message, to=robot_id)
            self.assertEqual(await tap.next(since=start), (peer.id, message, False))
            await peer.send("rawjobs", message, to=robot_id)  # a re-send is the application's to recognise
            await asyncio.sleep(QUIET_S)
            self.assertEqual(tap.since(start), [(peer.id, message, False)] * 2)
            self.assertEqual(peer.since(start), [], "fleet_agent answered on a raw channel")

            tap.send({"ack": 7}, to=peer.id)
            self.assertEqual(await peer.next(since=start), ("rawjobs", robot_id, {"ack": 7}))

        self._run(server, body)

    def test_6_a_bad_out_message_is_dropped_and_the_next_one_still_goes(self, server):
        tap = self.taps["status"]

        async def body(peer, robot_id):
            start = time.monotonic()
            tap.send_text("{not json", to=peer.id)
            tap.send_text("NaN", to=peer.id)  # Python would parse it; it is not JSON
            tap.send_text("", to=peer.id)
            tap.send({"lost": True}, to="r_nobody")  # not connected: the server says not_found
            tap.send({"ok": 1}, to=peer.id)
            self.assertEqual(await peer.next(since=start), ("status", robot_id, {"ok": 1}))
            await asyncio.sleep(QUIET_S)
            self.assertEqual(peer.since(start), [("status", robot_id, {"ok": 1})])

        self._run(server, body)

    def test_7_the_go_example_dispatcher_hands_jobs_to_the_application(self, server):
        binary = harness.go_dispatcher(server.workdir)
        if binary is None:
            self.skipTest("needs a Go toolchain to build sdk/go/examples/dispatcher")
        tap = self.taps["jobs"]

        async def body(peer, robot_id):
            start = time.monotonic()
            env = {
                **os.environ,
                "FLEET_URL": server.ws_url,
                "FLEET_ENROLL_KEY": server.enroll_key,
                "FLEET_TOKEN_FILE": str(server.workdir / "dispatcher.token.json"),
            }
            log_path = server.workdir / "dispatcher.log"
            with open(log_path, "wb") as log:
                dispatcher = subprocess.Popen([str(binary)], env=env, stdout=log, stderr=subprocess.STDOUT)
            try:
                # It makes a job every 2 s and sends it, acked, to a robot that is online and AUTONOMOUS.
                for n in (1, 2):
                    sender, job, acked = await tap.next(lambda m, n=n: m[1].get("id") == f"job-{n}", since=start)
                    self.assertEqual((job, acked), ({"id": f"job-{n}", "task": "inspect"}, True))
                    self.assertTrue(sender.startswith("s_"), sender)
                await asyncio.sleep(QUIET_S)
            finally:
                dispatcher.terminate()
                dispatcher.wait(timeout=5)
            output = log_path.read_text()
            print(f"dispatcher output:\n{output}")
            print(f"application node received on {tap.in_topic}: {tap.since(start)}")
            # The dispatcher heard the ack that fleet_agent sent for the application...
            for n in (1, 2):
                self.assertIn(f"acked job-{n} by {robot_id}", output)
            self.assertNotIn("requeue", output)
            # ...and the application saw each job exactly once.
            ids = [m[1]["id"] for m in tap.since(start)]
            self.assertEqual(sorted(set(ids)), sorted(ids), ids)

        self._run(server, body)

    def test_8_request_help_raises_the_hand_and_says_when_it_could_not(self, server):
        rec = self.rec

        async def scenario():
            async with harness.watching(server) as watcher, harness.operating(server) as op:
                robot = await watcher.robot(NAME, lambda r: r["state"] == "AUTONOMOUS")
                await rec.lease(lambda m: m.connected)

                # Refused without asking the server, with the code the server would use.
                for reason, context in (("", ""), ("x" * 257, ""), ("stuck", "[1, 2]"), ("stuck", "{nope")):
                    refused = await harness.request_help(rec.node, reason, context)
                    self.assertEqual((refused.accepted, refused.code), (False, "invalid_message"), refused)
                    self.assertTrue(refused.message)
                still = await watcher.robot(NAME)
                self.assertEqual(still["state"], "AUTONOMOUS")

                start = time.monotonic()
                asked = await harness.request_help(rec.node, "nav_goal_failed", '{"attempts": 3, "leg": "w12-w13"}')
                self.assertEqual((asked.accepted, asked.code), (True, ""), asked)
                waiting = await watcher.robot(NAME, lambda r: r["state"] == "HELP_REQUESTED")
                self.assertEqual(waiting["help"]["reason"], "nav_goal_failed")
                self.assertEqual(waiting["help"]["context"], {"attempts": 3, "leg": "w12-w13"})
                # The application can read the same from the latched lease state.
                raised = await rec.lease(lambda m: m.mode == "help", since=start)
                self.assertFalse(raised.held)
                # The longest reason the server takes, and asking twice, are both fine.
                again = await harness.request_help(rec.node, "y" * 256)
                self.assertTrue(again.accepted, again)
                kept = await watcher.robot(NAME)
                self.assertEqual(kept["help"]["reason"], "nav_goal_failed")  # the first request stands

                # An operator answers the call and hands back: the robot is autonomous again.
                lease = await op.claim(robot["robot_id"])
                await rec.lease(lambda m: m.held and m.mode == "teleop", since=start)
                await op.release(lease)
                await rec.lease(lambda m: not m.held and m.mode == "autonomous", since=start)

            # No server: the service says so at once instead of waiting for one.
            server.down()
            try:
                await rec.lease(lambda m: not m.connected, since=start)
                began = time.monotonic()
                offline = await harness.request_help(rec.node, "stuck")
                took = time.monotonic() - began
                self.assertEqual((offline.accepted, offline.code), (False, "offline"), offline)
                self.assertLess(took, 1.0)
            finally:
                server.up()
            await rec.lease(lambda m: m.connected, since=began)
            async with harness.watching(server) as watcher:
                back = await watcher.robot(NAME)
                self.assertEqual(back["state"], "AUTONOMOUS")  # the refused request was not queued
                asked = await harness.request_help(rec.node, "stuck_again")
                self.assertTrue(asked.accepted, asked)
                await watcher.robot(NAME, lambda r: r["state"] == "HELP_REQUESTED")

        asyncio.run(scenario())
