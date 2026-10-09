# Teleop lease state of this robot, as fleet_agent sees it. Published latched
# (reliable, transient local, depth 1) on fleet/lease whenever it changes, so a
# node that starts late still gets the current state.
#
# An application node uses it to hand the base over and take it back: cancel the
# active goal when `held` becomes true, resume or replan when it becomes false.

string MODE_AUTONOMOUS = "autonomous"
string MODE_HELP = "help"
string MODE_TELEOP = "teleop"

string REASON_RELEASED = "released"
string REASON_EXPIRED = "expired"
string REASON_STOLEN = "stolen"
string REASON_OPERATOR_LOST = "operator_lost"

# When this state was published (the node's clock).
builtin_interfaces/Time stamp

# True while an operator holds the lease: operator twist is being forwarded.
bool held

# The lease held now, or the one that just ended. Empty before the first lease.
string lease_id

# Who is driving. Set only while `held`.
string operator_id

# Why the last lease ended (a REASON_* value, or a newer one this file does not
# list yet). Empty while `held` and before the first lease. Also empty when the
# lease ended while the robot was disconnected (the server restarted, or the
# lease ran out during a network drop): the robot learns on reconnecting that
# it is gone, not why. `lease_id` then still names the lease that ended.
string reason

# The robot's intervention state, a MODE_* value: teleop while held; after a
# lease ends, autonomous if the operator handed back, help if it was lost (for
# any other reason, or for none known). Also help after fleet/request_help.
string mode

# False while the link to fleet-server is down. A lease survives a short drop
# (`held` stays true), but no twist can arrive until the link is back.
bool connected
