# One message on a fleet channel, as it crosses between fleet_agent and the
# robot's application node. For each channel <name> listed in fleet_agent's
# `channels` parameter there are two topics (reliable, keep last 10):
#
#   fleet/ch/<name>/in    fleet_agent publishes what arrived for this robot
#   fleet/ch/<name>/out   the application node publishes; fleet_agent sends it
#
# The platform never looks inside channel data, and neither does fleet_agent:
# `data` is the JSON text of the value, whatever it is (object, array, string,
# number, true, false, null). Parse it with json.loads / nlohmann::json.

# in: when fleet_agent received it (the node's clock). out: ignored.
builtin_interfaces/Time stamp

# in: client id of the sender (r_..., s_..., o_...), stamped by fleet-server.
#     Reply by publishing on the out topic with `to` set to this.
# out: ignored; the server stamps this robot's id.
string sender

# out: client id to send to; empty broadcasts to the channel (robots that
#      declare it and clients subscribed to it; never back to this robot).
# in: always empty. A message does not say whether it was sent to this robot
#     alone or broadcast.
string to

# The channel data as JSON text. On the out topic it must be valid JSON;
# fleet_agent drops anything else with a warning.
string data

# in: true when the sender used an acked send and fleet_agent answered it on the
#     application's behalf: `data` is then the inner value, delivered once per
#     send, and the sender is told it was accepted. False for ordinary channel
#     data, and always on a channel listed in `raw_channels`.
# out: ignored.
bool acked
