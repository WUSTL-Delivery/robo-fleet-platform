"""fleet_agent: the generic ROS 2 node that puts a robot on a fleet-server.

Configured, not programmed (see config/fleet_agent.example.yaml). The node is a
thin layer over the Python SDK (sdk/python, ``fleet``): the SDK owns the
socket, enrollment, heartbeat and reconnect; this package maps ROS parameters
and topics onto it.
"""

__version__ = "0.0.1"
